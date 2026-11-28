package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/host"
)

const hostName = "codex"

var threadID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// Driver 管理一个 daemon 的 app-server，其会话独立、连接共享。
type Driver struct{ s *server }

// New 创建惰性连接，首次请求时才启动 app-server。空程序名使用 daemon PATH 中的 codex。
func New(program string) *Driver {
	if program == "" {
		program = "codex"
	}
	return &Driver{s: newServer(program)}
}

// Capabilities 声明两步建会话；忙时追加仅对本实例有写入权的线程成立。
func (*Driver) Capabilities() host.Capabilities {
	return host.Capabilities{CreateSeparatesPrompt: true, BusyAppend: true, StopHook: true, SessionStatus: true}
}

// Close 停止接收新请求并回收 stdio、进程树与观察者。
func (d *Driver) Close() error { return d.s.close() }

func valid(s host.Session) bool { return s.Host == hostName && threadID.MatchString(s.ID) }

func sandbox(p host.Permissions) (string, error) {
	if p.NoNetwork || p.Tools != nil {
		return "", fmt.Errorf("%w: 宿主不能保证断网或工具白名单", host.Unsupported)
	}
	switch p.WriteScope {
	case host.ReadOnly:
		return "read-only", nil
	case "", host.Workspace:
		return "workspace-write", nil
	default:
		return "", fmt.Errorf("%w: 无法保证指定的写入范围", host.Unsupported)
	}
}

type turn struct{ ID, Status string }
type thread struct {
	ID     string
	Name   string
	Cwd    string
	Status json.RawMessage
	Turns  []turn
}
type response struct {
	Thread   thread
	Turn     turn
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ID       string
}

func receipt(primary, legacy, id string) string {
	for _, v := range []string{primary, legacy, id} {
		if threadID.MatchString(v) {
			return v
		}
	}
	return ""
}

func statusName(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		var obj struct{ Type string }
		if json.Unmarshal(raw, &obj) != nil {
			return ""
		}
		value = obj.Type
	}
	return strings.ToLower(value)
}

func state(raw json.RawMessage) host.State {
	switch statusName(raw) {
	case "busy", "active", "running", "in_progress", "inprogress", "waiting_person", "waiting", "approval":
		return host.Busy
	case "idle", "completed", "interrupted", "ready":
		return host.Idle
	case "gone", "archived", "deleted":
		return host.Gone
	default:
		return host.UnknownState
	}
}

func (d *Driver) read(ctx context.Context, s host.Session, turns bool) (thread, error) {
	var r response
	err := d.s.call(ctx, "thread/read", map[string]any{"threadId": s.ID, "includeTurns": turns}, &r)
	if err == nil && r.Thread.ID != s.ID {
		err = errors.New("thread/read 缺少匹配的线程")
	}
	return r.Thread, err
}

// Status 查询当前线程，无需传输历史。未载入、其他 writer、断线均不能证明会话已消失。
func (d *Driver) Status(ctx context.Context, s host.Session) host.State {
	if !valid(s) {
		return host.UnknownState
	}
	th, err := d.read(ctx, s, false)
	if err != nil {
		return host.UnknownState
	}
	return state(th.Status)
}

func ownerConflict(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "active writer")
}
func unloaded(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "not loaded")
}

func (d *Driver) resume(ctx context.Context, s host.Session) (thread, error) {
	var r response
	err := d.s.call(ctx, "thread/resume", map[string]string{"threadId": s.ID}, &r)
	if err == nil && r.Thread.ID != s.ID {
		return thread{}, errors.New("thread/resume 缺少匹配的线程")
	}
	return r.Thread, err
}

func activeID(th thread) string {
	for i := len(th.Turns) - 1; i >= 0; i-- {
		t := th.Turns[i]
		switch strings.ToLower(t.Status) {
		case "inprogress", "in_progress", "active", "running":
			if threadID.MatchString(t.ID) {
				return t.ID
			}
		}
	}
	return ""
}

// Append 在写之前查明当前轮；明确未载入时才续接。写请求丢失回执后绝不自动重发。
func (d *Driver) Append(ctx context.Context, s host.Session, text, id string) host.Result {
	marked, err := host.MarkDelivery(id, text)
	if err != nil || !valid(s) {
		return host.Result{Outcome: host.Unsupported, Err: host.Unsupported}
	}
	th, err := d.read(ctx, s, false)
	if unloaded(err) || err == nil && statusName(th.Status) == "notloaded" {
		th, err = d.resume(ctx, s)
	}
	if err != nil {
		return host.Result{Outcome: host.Later, Err: err}
	}
	got, err := d.offer(ctx, s, th, marked)
	if unloaded(err) && !errors.Is(err, host.Later) {
		// 读得到持久历史不等于当前连接已装入线程。仅结构化拒收后续接一次。
		th, err = d.resume(ctx, s)
		if err != nil {
			return host.Result{Outcome: host.Later, Err: err}
		}
		got, err = d.offer(ctx, s, th, marked)
	}
	if ownerConflict(err) || errors.Is(err, host.Later) || unloaded(err) {
		return host.Result{Outcome: host.Later, Err: err}
	}
	if err != nil || got == "" {
		if err == nil {
			err = errors.New("缺少轮次 ID")
		}
		return host.Result{Outcome: host.Unknown, Err: fmt.Errorf("请在 codex://threads/%s 核对投递标记 %s: %w", s.ID, id, err)}
	}
	return host.Result{Outcome: host.OK}
}

func (d *Driver) offer(ctx context.Context, s host.Session, th thread, marked string) (string, error) {
	input := map[string]any{"threadId": s.ID, "input": []map[string]string{{"type": "text", "text": marked}}}
	method := "turn/start"
	expected := ""
	switch state(th.Status) {
	case host.Busy:
		var err error
		th, err = d.read(ctx, s, true)
		if err != nil {
			return "", errors.Join(host.Later, err)
		}
		expected = activeID(th)
		if expected == "" {
			return "", host.Later
		}
		input["expectedTurnId"] = expected
		method = "turn/steer"
	case host.Idle:
	default:
		return "", fmt.Errorf("%w: 尚未取得可投递的线程状态", host.Later)
	}
	var r response
	err := d.s.call(ctx, method, input, &r)
	id := receipt(r.Turn.ID, r.TurnID, r.ID)
	if err == nil && expected != "" && id != expected {
		return "", errors.New("续轮回执的轮次不符")
	}
	return id, err
}

// Stop 中断指定轮次。空 RPC 回执只表示接受，必须再从宿主确认空闲或已消失。
func (d *Driver) Stop(ctx context.Context, s host.Session, timeout time.Duration) error {
	if !valid(s) {
		return host.Unsupported
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	th, err := d.read(ctx, s, true)
	if unloaded(err) || err == nil && statusName(th.Status) == "notloaded" {
		th, err = d.resume(ctx, s)
	}
	if err != nil {
		return fmt.Errorf("%w: 核对运行轮次: %w", host.Unknown, err)
	}
	if st := state(th.Status); st == host.Idle || st == host.Gone {
		return nil
	}
	id := activeID(th)
	if id == "" {
		return fmt.Errorf("%w: 无法确定会话 %s 的运行轮次", host.Unknown, s.ID)
	}
	if err := d.s.call(ctx, "turn/interrupt", map[string]string{"threadId": s.ID, "turnId": id}, nil); err != nil {
		return fmt.Errorf("%w: 核对中断结果: %w", host.Unknown, err)
	}
	for {
		if st := d.Status(ctx, s); st == host.Idle || st == host.Gone {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: 核对会话 %s 是否空闲: %w", host.Unknown, s.ID, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// FindByTitle 列出工作目录与标题精确匹配的候选。标题可重名，不能用来单独证明归属或发首条。
func (d *Driver) FindByTitle(ctx context.Context, dir, title string) ([]host.Session, error) {
	if title == "" || !filepath.IsAbs(dir) {
		return nil, host.Unsupported
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var matches []host.Session
	params := map[string]any{"limit": 100}
	seen := map[string]bool{}
	for {
		var list struct {
			Data []thread
			Next string `json:"nextCursor"`
		}
		if err := d.s.call(ctx, "thread/list", params, &list); err != nil {
			return nil, fmt.Errorf("按标题对账: %w", err)
		}
		if list.Data == nil {
			return nil, fmt.Errorf("%w: 对账清单无效", host.Unknown)
		}
		for _, th := range list.Data {
			if th.Name == title && filepath.Clean(th.Cwd) == filepath.Clean(dir) && threadID.MatchString(th.ID) {
				matches = append(matches, host.Session{Host: hostName, ID: th.ID})
			}
		}
		if list.Next == "" {
			return matches, nil
		}
		if seen[list.Next] {
			return nil, fmt.Errorf("%w: 对账清单游标重复", host.Unknown)
		}
		seen[list.Next] = true
		params["cursor"] = list.Next
	}
}

// LastReply 未有持久会话回复读取的实测契约，由调用方提示人打开会话。
func (*Driver) LastReply(context.Context, host.Session) (string, error) { return "", host.Unsupported }

// Open 返回可供人打开的目标 URI；daemon 自身没有已验证的图形入口。
func (*Driver) Open(_ context.Context, s host.Session) error {
	if !valid(s) {
		return host.Unsupported
	}
	return fmt.Errorf("%w: 请打开 codex://threads/%s", host.Unsupported, s.ID)
}

// WorktreePath 允许在仓库的独立任务目录中创建 detached worktree。
func (*Driver) WorktreePath(repo, name string) string { return filepath.Join(repo, ".worktrees", name) }

// Statuses 只取线程清单。清单没有某项不能证明它已消失，按 unknown 恢复。
func (d *Driver) Statuses(ctx context.Context, sessions []host.Session) map[host.Session]host.State {
	result := make(map[host.Session]host.State, len(sessions))
	for _, s := range sessions {
		result[s] = host.UnknownState
	}
	params := map[string]any{"limit": 100}
	seen := map[string]bool{}
	for {
		var page struct {
			Data []thread
			Next string `json:"nextCursor"`
		}
		if err := d.s.call(ctx, "thread/list", params, &page); err != nil {
			return result
		}
		for _, th := range page.Data {
			s := host.Session{Host: hostName, ID: th.ID}
			if _, ok := result[s]; ok {
				switch statusName(th.Status) {
				case "waiting_person", "waiting", "approval":
					result[s] = host.WaitingPerson
				default:
					result[s] = state(th.Status)
				}
			}
		}
		if page.Next == "" || seen[page.Next] {
			return result
		}
		seen[page.Next] = true
		params["cursor"] = page.Next
	}
}

// Rename 对已登记线程同步显示标题；线程归属必须先由调用方按会话 ID 核对。
func (d *Driver) Rename(ctx context.Context, s host.Session, title string) error {
	if !valid(s) || title == "" {
		return host.Unsupported
	}
	return d.s.call(ctx, "thread/name/set", map[string]string{"threadId": s.ID, "name": title}, nil)
}
