package app

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/store"
)

// AlarmRequest 记录一次立即触发；调用进程可按 RequestID 在独立连接上等候终态。
type AlarmRequest struct {
	RequestID string `json:"requestId"`
	Alarm     string `json:"alarm"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
}

// EnqueueAlarm 仅将请求持久入队。即使周期被禁用，daemon 也会尝试显式触发。
func EnqueueAlarm(ctx context.Context, db *store.SQLite, ext, id string) (string, error) {
	if ext == "" || id == "" || strings.Contains(ext, ":") || strings.Contains(id, ":") {
		return "", fmt.Errorf("扩展或闹钟标识无效")
	}
	req := AlarmRequest{RequestID: rand.Text(), Alarm: ext + ":" + id, Status: "queued"}
	err := db.Write(ctx, func(tx *store.SQLiteTx) error { return req.save(tx) })
	if err != nil {
		return "", fmt.Errorf("登记闹钟请求: %w", err)
	}
	return req.RequestID, nil
}

func (req AlarmRequest) save(tx *store.SQLiteTx) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return tx.Put("watch_alarm_request", req.RequestID, raw)
}

// ReadAlarmRequest 读取当前状态，等待方的 ctx 只影响此次读取。
func ReadAlarmRequest(ctx context.Context, db *store.SQLite, id string) (AlarmRequest, error) {
	var req AlarmRequest
	err := db.View(ctx, func(r store.SQLiteReader) error {
		var err error
		req, err = alarmRequest(r, id)
		return err
	})
	return req, err
}

func alarmRequest(r store.SQLiteReader, id string) (AlarmRequest, error) {
	var req AlarmRequest
	v, ok, err := r.Get("watch_alarm_request", id)
	if err != nil {
		return req, err
	}
	if !ok {
		return req, fmt.Errorf("请求 %q 不存在", id)
	}
	err = json.Unmarshal(v, &req)
	return req, err
}

type requestedCall struct {
	req      AlarmRequest
	ctx      context.Context
	returned bool
	err      error
}

type alarmQueue struct {
	db         *store.SQLite
	service    *alarm.Service
	registered map[string]bool
	// Trigger 接纳后，Run 才异步取得该请求。周期回调永远不能认领 requested 的结果。
	mu    sync.Mutex
	armed map[string]*requestedCall
	calls map[string]*requestedCall // 仅消费循环与其退出后的清理步骤访问。
}

func newAlarmQueue(db *store.SQLite, s *alarm.Service) *alarmQueue {
	return &alarmQueue{db: db, service: s, registered: map[string]bool{}, armed: map[string]*requestedCall{}, calls: map[string]*requestedCall{}}
}

func (q *alarmQueue) wrap(id string, run func(context.Context, alarm.RunRequest) error) func(context.Context, alarm.RunRequest) error {
	q.registered[id] = true
	return func(ctx context.Context, req alarm.RunRequest) error {
		q.mu.Lock()
		call := q.armed[id]
		delete(q.armed, id)
		if call != nil {
			if req.Reason == "requested" {
				call.ctx = ctx
			} else {
				call.req.Status = "failed"
				call.req.Detail = "调用开始前已取消"
				call = nil
			}
		}
		q.mu.Unlock()
		err := run(ctx, req)
		if call != nil {
			q.mu.Lock()
			call.err = err
			call.returned = true
			q.mu.Unlock()
		}
		return err
	}
}

func (q *alarmQueue) trigger(req AlarmRequest) {
	call := &requestedCall{req: req}
	q.calls[req.RequestID] = call
	if !q.registered[req.Alarm] {
		call.req.Status = "failed"
		call.req.Detail = "闹钟未登记"
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := q.service.Trigger(req.Alarm); err != nil {
		call.req.Status = "skipped"
		call.req.Detail = err.Error()
		return
	}
	if prev := q.armed[req.Alarm]; prev != nil {
		// 服务可能在 Run 被调到之前已判超时。新请求获准说明旧名额已释放。
		prev.req.Status = "failed"
		prev.req.Detail = "调用开始前已取消"
	}
	q.armed[req.Alarm] = call
}

// poll 只有 daemon 的单个循环调用。先提交 running 再调用扩展，崩溃恢复时不重放结果未知的外部动作。
func (q *alarmQueue) poll(ctx context.Context) error {
	// 空闲时只读，不竞争写锁。取得写锁后重新选择队首，认领和置 running 同时提交。
	head, err := q.rows(ctx, "queued", 1)
	if err != nil {
		return err
	}
	if len(head) > 0 {
		var batch []AlarmRequest
		if err := q.db.Write(ctx, func(tx *store.SQLiteTx) error {
			rows, err := tx.Lookup("requests", "queued", batchLimit)
			if err != nil {
				return err
			}
			for _, doc := range rows {
				var req AlarmRequest
				if err := json.Unmarshal(doc.Value, &req); err != nil {
					return err
				}
				req.Status = "running"
				if err := req.save(tx); err != nil {
					return err
				}
				batch = append(batch, req)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, req := range batch {
			q.trigger(req)
		}
	}
	return q.flush(ctx)
}

func (q *alarmQueue) rows(ctx context.Context, state string, limit int) ([]store.Doc, error) {
	var rows []store.Doc
	err := q.db.View(ctx, func(r store.SQLiteReader) error {
		var err error
		rows, err = r.Lookup("requests", state, limit)
		return err
	})
	return rows, err
}

func (q *alarmQueue) recover(ctx context.Context) error {
	for {
		rows, err := q.rows(ctx, "running", batchLimit)
		if err != nil || len(rows) == 0 {
			return err
		}
		if err = q.db.Write(ctx, func(tx *store.SQLiteTx) error {
			for _, doc := range rows {
				var req AlarmRequest
				if err := json.Unmarshal(doc.Value, &req); err != nil {
					return err
				}
				req.Status, req.Detail = "failed", "上次进程中断，执行结果未知"
				if err := req.save(tx); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
}

func (q *alarmQueue) flush(ctx context.Context) error {
	if len(q.calls) == 0 {
		return nil
	}
	q.mu.Lock()
	active := map[string]bool{}
	for _, s := range q.service.List() {
		active[s.ID] = s.State == "running"
	}
	var finished []AlarmRequest
	for _, c := range q.calls {
		if req := q.result(c, active[c.req.Alarm]); req.Status != "running" {
			finished = append(finished, req)
		}
	}
	q.mu.Unlock()
	if len(finished) == 0 {
		return nil
	}
	if err := q.db.Write(ctx, func(tx *store.SQLiteTx) error {
		for _, req := range finished {
			if err := req.save(tx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// 失败的落盘在下一拍重试。按已提交 ID 清除，不能顺便移除刚结束的并发回调。
	for _, req := range finished {
		delete(q.calls, req.RequestID)
	}
	return nil
}

func (q *alarmQueue) result(c *requestedCall, active bool) AlarmRequest {
	r := c.req
	if r.Status != "running" {
		return r
	}
	if c.ctx == nil {
		if !active {
			if q.armed[r.Alarm] == c {
				delete(q.armed, r.Alarm)
			}
			c.req.Status, c.req.Detail = "failed", "调用开始前已取消"
			return c.req
		}
		return r
	}
	// 服务在正常返回后也会取消运行 context。等这个信号才能区分恰好卡在截止时刻的成功与超时。
	if c.ctx.Err() == nil {
		return r
	}
	cause := context.Cause(c.ctx)
	switch {
	case errors.Is(cause, context.DeadlineExceeded) || !c.returned:
		r.Status, r.Detail = "failed", cause.Error()
	case c.err != nil:
		r.Status, r.Detail = "failed", c.err.Error()
	default:
		r.Status = "done"
	}
	return r
}
