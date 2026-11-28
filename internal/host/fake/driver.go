// Package fake 提供离线会话驱动。每种操作有自己的脚本，便于分别控制投递与监视。
package fake

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/host"
)

// Op 用于编排与断言宿主调用。
type Op string

// 四种有副作用或需采样的操作可以分别编排。
const (
	Create      Op = "create"
	Append      Op = "append"
	Stop        Op = "stop"
	Status      Op = "status"
	StatusBatch Op = "statuses"
	Rename      Op = "rename"
)

// Step 指定一次调用的延迟、回执及观察到的状态。Create 的 ID 可模拟回调登记后丢失回执。
type Step struct {
	Op      Op
	Delay   time.Duration
	Outcome host.Outcome
	State   host.State
	Reply   string
	ID      string
	Err     error
}

// Call 是开始调用时的快照。Text 保留实际发出的标记；重复投递仍记调用次数。
type Call struct {
	Op         Op
	Spec       *host.CreateSpec
	Session    host.Session
	Text       string
	DeliveryID string
	Sessions   []host.Session
}

// Driver 可并发调用。成功收到的投递按会话去重，Unknown 不自动补发。
type Driver struct {
	caps  host.Capabilities
	after func(time.Duration) <-chan time.Time

	mu       sync.Mutex
	script   map[Op][]Step
	calls    []Call
	sequence int
	sessions map[host.Session]host.State
	received map[host.Session]map[string]bool
	replies  map[host.Session]string
}

var _ host.Driver = (*Driver)(nil)

// New 创建隔离实例。after 用于延迟步骤，测试可在计时器登记后才推进时间。
func New(caps host.Capabilities, after func(time.Duration) <-chan time.Time) *Driver {
	if after == nil {
		after = time.After
	}
	return &Driver{caps: caps, after: after, script: make(map[Op][]Step),
		sessions: make(map[host.Session]host.State), received: make(map[host.Session]map[string]bool), replies: make(map[host.Session]string)}
}

// Script 追加脚本；同种操作按进入顺序消费，允许在测试途中添加步骤。
func (d *Driver) Script(steps ...Step) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range steps {
		d.script[s.Op] = append(d.script[s.Op], s)
	}
}

// Calls 返回独立快照，等待中的调用也已被记录。
func (d *Driver) Calls() []Call {
	d.mu.Lock()
	defer d.mu.Unlock()
	calls := slices.Clone(d.calls)
	for i, c := range calls {
		calls[i].Sessions = slices.Clone(c.Sessions)
		if c.Spec != nil {
			calls[i].Spec = snapshot(*c.Spec)
		}
	}
	return calls
}

func snapshot(spec host.CreateSpec) *host.CreateSpec {
	spec.OnCreated = nil
	spec.Env = maps.Clone(spec.Env)
	spec.Permissions.Tools = slices.Clone(spec.Permissions.Tools)
	return &spec
}

func (d *Driver) begin(ctx context.Context, c Call, limit time.Duration) (Step, error) {
	d.mu.Lock()
	d.calls = append(d.calls, c)
	var s Step
	if q := d.script[c.Op]; len(q) > 0 {
		s, d.script[c.Op] = q[0], q[1:]
	}
	d.mu.Unlock()
	delay := s.Delay
	timedOut := c.Op == Stop && delay >= limit
	if timedOut {
		delay = limit
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return s, ctx.Err()
		case <-d.after(delay):
		}
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	if timedOut {
		return s, context.DeadlineExceeded
	}
	return s, nil
}

// observe 在锁内应用脚本给出的宿主事实，轮次结果与投递回执可分别控制。
func (d *Driver) observe(session host.Session, s Step) {
	if s.State != "" {
		d.sessions[session] = s.State
	}
	if s.Reply != "" {
		d.replies[session] = s.Reply
	}
}

// Capabilities 返回上层在本次场景中应采用的能力表。
func (d *Driver) Capabilities() host.Capabilities { return d.caps }

// Create 在返回会话前同步通知调用方。未指定 ID 的未知创建不触发登记。
func (d *Driver) Create(ctx context.Context, spec host.CreateSpec) (host.Session, error) {
	text := spec.Prompt
	if spec.DeliveryID != "" || text != "" {
		var err error
		text, err = host.MarkDelivery(spec.DeliveryID, text)
		if err != nil {
			return host.Session{}, err
		}
	}
	s, err := d.begin(ctx, Call{Op: Create, Spec: snapshot(spec), Text: text, DeliveryID: spec.DeliveryID}, 0)
	if err != nil {
		return host.Session{}, err
	}
	if s.Outcome != "" && s.Outcome != host.OK && (s.Outcome != host.Unknown || s.ID == "") {
		return host.Session{}, errors.Join(s.Outcome, s.Err)
	}
	if s.Err != nil && s.Outcome != host.Unknown {
		return host.Session{}, s.Err
	}
	d.mu.Lock()
	d.sequence++
	id := s.ID
	if id == "" {
		id = strconv.Itoa(d.sequence)
	}
	session := host.Session{Host: "fake", ID: id}
	d.sessions[session] = host.Idle
	d.received[session] = map[string]bool{}
	d.mu.Unlock()
	if spec.OnCreated != nil {
		if err := spec.OnCreated(session); err != nil {
			return session, fmt.Errorf("%w: 登记会话: %w", host.Unknown, err)
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observe(session, s)
	if s.Outcome == host.Unknown {
		return session, errors.Join(s.Outcome, s.Err)
	}
	if spec.DeliveryID != "" {
		d.received[session][spec.DeliveryID] = true
	}
	return session, nil
}

// Append 使用生产标记函数。同一标记确认成功后，重复的调用不会再开始一轮。
func (d *Driver) Append(ctx context.Context, session host.Session, text, id string) host.Result {
	text, err := host.MarkDelivery(id, text)
	if err != nil {
		return host.Result{Outcome: host.Unsupported, Err: err}
	}
	s, err := d.begin(ctx, Call{Op: Append, Session: session, Text: text, DeliveryID: id}, 0)
	if err != nil {
		return host.Result{Outcome: host.Unknown, Err: err}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observe(session, s)
	if s.Outcome != "" && s.Outcome != host.OK {
		return host.Result{Outcome: s.Outcome, Err: errors.Join(s.Outcome, s.Err)}
	}
	if s.Err != nil {
		return host.Result{Outcome: host.Unknown, Err: s.Err}
	}
	if d.received[session] == nil || d.sessions[session] == host.Gone {
		return host.Result{Outcome: host.Unknown, Err: fmt.Errorf("会话已丢失，请核对 %s", id)}
	}
	if d.received[session][id] {
		return host.Result{Outcome: host.OK}
	}
	if d.sessions[session] == host.Busy && !d.caps.BusyAppend && s.Outcome == "" {
		return host.Result{Outcome: host.Later}
	}
	d.received[session][id] = true
	if s.State == "" {
		d.sessions[session] = host.Busy
	}
	return host.Result{Outcome: host.OK}
}

// Stop 停止当前轮而保留会话。延迟超出调用方时限时返回 Unknown。
func (d *Driver) Stop(ctx context.Context, session host.Session, timeout time.Duration) error {
	s, err := d.begin(ctx, Call{Op: Stop, Session: session}, timeout)
	if err != nil {
		return fmt.Errorf("%w: 停止会话: %w", host.Unknown, err)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observe(session, s)
	if s.Outcome != "" && s.Outcome != host.OK {
		return errors.Join(s.Outcome, s.Err)
	}
	if s.Err != nil {
		return s.Err
	}
	if s.State == "" && d.sessions[session] != "" && d.sessions[session] != host.Gone {
		d.sessions[session] = host.Idle
	}
	return nil
}

// Status 按脚本更新状态；丢失的会话在以后采样时仍为 Gone。
func (d *Driver) Status(ctx context.Context, session host.Session) host.State {
	s, err := d.begin(ctx, Call{Op: Status, Session: session}, 0)
	if err != nil || s.Err != nil || (s.Outcome != "" && s.Outcome != host.OK) {
		return host.UnknownState
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.observe(session, s)
	if state, ok := d.sessions[session]; ok {
		return state
	}
	return host.Gone
}

// LastReply 返回脚本指定的最近回复；也可通过能力表测试读取降级。
func (d *Driver) LastReply(_ context.Context, session host.Session) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if reply, ok := d.replies[session]; ok && d.caps.LastReply {
		return reply, nil
	}
	return "", host.Unsupported
}

// Open 供调用者测试没有人工查看入口时的降级。
func (*Driver) Open(context.Context, host.Session) error { return host.Unsupported }

// WorktreePath 允许在仓库的独立任务目录中创建 detached worktree。
func (*Driver) WorktreePath(repo, name string) string { return filepath.Join(repo, ".worktrees", name) }

// Statuses 用一次调用快照标记监视拍，各会话按请求顺序消费脚本。
func (d *Driver) Statuses(ctx context.Context, sessions []host.Session) map[host.Session]host.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, Call{Op: StatusBatch, Sessions: slices.Clone(sessions)})
	states := make(map[host.Session]host.State, len(sessions))
	for _, session := range sessions {
		if q := d.script[Status]; len(q) > 0 {
			step := q[0]
			d.script[Status] = q[1:]
			if ctx.Err() != nil || step.Err != nil || step.Outcome != "" && step.Outcome != host.OK {
				states[session] = host.UnknownState
				continue
			}
			d.observe(session, step)
		}
		state, ok := d.sessions[session]
		if !ok {
			state = host.Gone
		}
		states[session] = state
	}
	return states
}

// Rename 记录标题变化，测试可核对退回对象后使用的会话号。
func (d *Driver) Rename(ctx context.Context, s host.Session, title string) error {
	_, err := d.begin(ctx, Call{Op: Rename, Session: s, Text: title}, 0)
	return err
}
