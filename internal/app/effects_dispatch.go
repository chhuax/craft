package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/store"
)

// stop 等 kind 可以没有业务限额；每类执行池仍保留物理上限，空槽不预建 goroutine。
const maxEffectWorkersPerKind = 64

// 以下状态归单次 Start 的调度循环独占，处理器仅传回执行结果。
type effectKind struct {
	handler      EffectHandler
	running      map[string]bool
	jobs         chan store.Effect
	workerCount  int
	health       HealthState
	retryWriteAt time.Time
}

type effectCompletion struct {
	kind   *effectKind
	row    store.Effect
	result EffectResult
}

type effectRun struct {
	*effectScheduler
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	finished chan effectCompletion
	workers  sync.WaitGroup
}

func (r *effectRun) loop() {
	defer close(r.done)
	defer r.workers.Wait()
	kinds := make([]effectKind, len(r.handlers))
	for i, h := range r.handlers {
		kinds[i] = effectKind{handler: h, running: make(map[string]bool), jobs: make(chan store.Effect, maxEffectWorkersPerKind)}
	}
	for r.ctx.Err() == nil {
		now := r.clock.Now()
		var next time.Time
		for i := range kinds {
			at := r.dispatch(&kinds[i], now)
			if !at.IsZero() && (next.IsZero() || at.Before(next)) {
				next = at
			}
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if !next.IsZero() {
			timer = r.clock.NewTimer(next.Sub(r.clock.Now()))
			tick = timer.C
		}
		select {
		case <-r.ctx.Done():
		case <-r.wake:
		case <-tick:
		case c := <-r.finished:
			// 写回完成后直接再调度一轮，相当于 Kick，名额与键一起释放。
			r.finish(c)
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (r *effectRun) dispatch(k *effectKind, now time.Time) time.Time {
	if r.ctx.Err() != nil {
		return time.Time{}
	}
	if now.Before(k.retryWriteAt) {
		return k.retryWriteAt
	}
	h := k.handler
	ordered := h.Ordered()
	limit := min(maxEffectWorkersPerKind, max(0, h.Limit()))
	if ordered {
		limit = min(1, limit)
	}
	stats := k.health.EffectStats
	var rows []store.Effect
	var next time.Time
	err := r.db.View(r.ctx, func(reader store.SQLiteReader) error {
		current, err := reader.EffectStats(h.Kind())
		if err != nil {
			return err
		}
		stats = current
		if limit <= len(k.running) || stats.Waiting == 0 {
			return nil
		}
		if ordered {
			rows, next, err = orderedEffect(reader, h.Kind(), now)
		} else if picker, ok := h.(interface {
			selectDue(store.SQLiteReader, time.Time, int) ([]store.Effect, time.Time, error)
		}); ok {
			rows, next, err = picker.selectDue(reader, now, limit)
		} else {
			rows, next, err = unorderedEffects(reader, h.Kind(), now, limit)
		}
		return err
	})
	if err != nil {
		r.reportError(k, stats, err)
		return time.Time{}
	}
	r.submit(k, rows, limit)
	state := HealthState{EffectStats: stats, Status: "ok"}
	if limit == 0 {
		state.Status = "paused"
	}
	if stats.MaxAttempts >= 3 && !h.Quiet() {
		state.Status = "error"
	}
	r.report(k, state)
	if len(k.running) >= limit {
		return time.Time{}
	}
	return next
}

func unorderedEffects(reader store.SQLiteReader, kind string, now time.Time, limit int) ([]store.Effect, time.Time, error) {
	// 已到期的前缀可能含正在执行的键。取到 limit 行才足以填满剩余名额。
	rows, err := reader.DueEffects(kind, now, limit)
	if err != nil || len(rows) == limit {
		return rows, time.Time{}, err
	}
	// 跳过所有已到期行，否则运行中的键会令空闲名额一直空转。
	next, err := reader.NextEffectDue(kind, now)
	return rows, next, err
}

func orderedEffect(reader store.SQLiteReader, kind string, now time.Time) ([]store.Effect, time.Time, error) {
	head, err := reader.HeadEffect(kind)
	if err != nil || head == nil {
		return nil, time.Time{}, err
	}
	due, err := time.Parse(time.RFC3339Nano, head.Due)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("解析 %s 队首的到期时间: %w", kind, err)
	}
	if due.After(now) {
		return nil, due, nil
	}
	return []store.Effect{*head}, time.Time{}, nil
}

func (r *effectRun) submit(k *effectKind, rows []store.Effect, limit int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, row := range rows {
		if len(k.running) >= limit || r.ctx.Err() != nil {
			return
		}
		if k.running[row.Key] {
			continue
		}
		// 在提交前预占键与名额，无需创建等候槽位的 goroutine。
		k.running[row.Key] = true
		if k.workerCount < len(k.running) {
			k.workerCount++
			r.workers.Go(func() { r.work(k) })
		}
		// 缓冲只接纳已预占名额的行，容量覆盖全部名额，提交不会在启停锁内等待。
		k.jobs <- row
	}
}

func (r *effectRun) work(k *effectKind) {
	for {
		select {
		case <-r.ctx.Done():
			return
		case row := <-k.jobs:
			if r.ctx.Err() != nil {
				return
			}
			c := effectCompletion{k, row, runEffect(r.ctx, k.handler, row)}
			select {
			case r.finished <- c:
			case <-r.ctx.Done():
				return
			}
		}
	}
}

func runEffect(ctx context.Context, h EffectHandler, row store.Effect) (result EffectResult) {
	// 处理器可能调用进程外端口。把其 panic 记为本行失败，其他行及其他 kind 仍能推进。
	defer func() {
		if p := recover(); p != nil {
			result = EffectResult{Err: fmt.Errorf("处理器 panic: %v", p)}
		}
	}()
	return h.Run(ctx, row)
}

func (r *effectRun) finish(c effectCompletion) {
	delete(c.kind.running, c.row.Key)
	if r.ctx.Err() != nil {
		return
	}
	// Err 由处理器提供，在取得写锁之前提取文本。
	var message string
	if c.result.Err != nil {
		message = fmt.Sprint(c.result.Err)
	}
	err := r.db.Write(r.ctx, func(tx *store.SQLiteTx) error {
		base := r.retryBase
		if c.row.Kind == "launch" {
			base = 5 * time.Second
		}
		switch {
		case !c.result.Due.IsZero():
			return tx.LaterEffect(c.row, c.result.Due, 0)
		case c.result.Err != nil:
			return tx.FailEffect(c.row, r.clock.Now(), base, r.retryMax, message)
		case c.result.Later:
			return tx.LaterEffect(c.row, r.clock.Now(), base)
		default:
			return tx.DeleteEffect(c.row)
		}
	})
	if err != nil {
		// 写回失败时旧行仍可能到期；留出恢复间隔，避免立即重复执行外部动作。
		c.kind.retryWriteAt = r.clock.Now().Add(r.retryBase)
		r.reportError(c.kind, c.kind.health.EffectStats, err)
	}
}

func (r *effectRun) reportError(k *effectKind, stats store.EffectStats, err error) {
	if r.ctx.Err() == nil {
		stats.LastError = err.Error()
		r.report(k, HealthState{EffectStats: stats, Status: "error"})
	}
}

func (r *effectRun) report(k *effectKind, state HealthState) {
	if state != k.health {
		k.health = state
		if r.onHealth != nil {
			r.onHealth(k.handler.Kind(), state)
		}
	}
}
