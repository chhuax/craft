// Package claudecode 使用 Claude Code 的后台会话清单及持久会话输入通道。
package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/host"
)

const hostName = host.ClaudeCode

var envPrefixes = []string{"ANTHROPIC_", "CLAUDE_"}

var uuid = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type agent struct {
	ID         string          `json:"id"`
	SessionID  string          `json:"sessionId"`
	Status     string          `json:"status"`
	State      string          `json:"state"`
	WaitingFor string          `json:"waitingFor"`
	PID        json.RawMessage `json:"pid"`
}

// Driver 合并同一批会话的全量状态查询。一个实例在 daemon 内共享。
type Driver struct {
	life       context.Context
	cancel     context.CancelFunc
	lookup     func(context.Context, host.Session) (host.CreateSpec, error)
	mu         sync.Mutex
	current    map[string]*stream
	replies    map[string]string
	specs      map[string]host.CreateSpec
	workers    sync.WaitGroup
	program    string
	now        func() time.Time
	listMu     sync.Mutex
	validUntil time.Time
	agents     map[string]agent
	listErr    error
}

// New 接受 daemon 选定的可执行文件。lookup 在续轮前从派工方取当前目录、绑定与权限，供重启或换代使用。
func New(program string, lookup func(context.Context, host.Session) (host.CreateSpec, error)) *Driver {
	if program == "" {
		program = "claude"
	}
	life, cancel := context.WithCancel(context.Background())
	return &Driver{program: program, now: time.Now, life: life, cancel: cancel, lookup: lookup,
		current: make(map[string]*stream), replies: make(map[string]string), specs: make(map[string]host.CreateSpec)}
}

func parseAgents(out string) (map[string]agent, error) {
	raw := bytes.TrimSpace([]byte(out))
	if len(raw) > 0 && raw[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		raw = nil
		for _, field := range []string{"agents", "sessions", "items"} {
			if value, ok := obj[field]; ok {
				raw = value
				break
			}
		}
	}
	var records []struct {
		agent
		UUID         string `json:"uuid"`
		SessionSnake string `json:"session_id"`
		ShortID      string `json:"shortId"`
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, err
	}
	if records == nil {
		return nil, errors.New("清单缺少会话数组")
	}
	index := make(map[string]agent, len(records))
	for _, r := range records {
		a := r.agent
		if a.SessionID == "" {
			a.SessionID = r.SessionSnake
		}
		if a.SessionID == "" {
			a.SessionID = r.UUID
		}
		if a.ID == "" {
			a.ID = r.ShortID
		}
		if !uuid.MatchString(a.SessionID) {
			return nil, errors.New("清单缺少完整会话 ID")
		}
		index[a.SessionID] = a
	}
	return index, nil
}

func (d *Driver) list(ctx context.Context, fresh bool) (map[string]agent, error) {
	d.listMu.Lock()
	defer d.listMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !fresh && d.now().Before(d.validUntil) {
		return d.agents, d.listErr
	}
	out, _, err := host.Run(ctx, d.program, "", []string{"agents", "--json", "--all"}, nil, "", 10*time.Second, envPrefixes...)
	if err == nil {
		d.agents, err = parseAgents(out)
	}
	d.listErr = err
	// 一次监视依次问多个会话。一秒内共享同一不可变快照，发消息及确认停止须强制刷新。
	d.validUntil = d.now().Add(time.Second)
	return d.agents, err
}

func (a agent) state() host.State {
	status, state := strings.ToLower(a.Status), strings.ToLower(a.State)
	switch {
	case status == "waiting" || a.WaitingFor != "":
		return host.Busy
	case state == "stopped" || state == "exited" || state == "killed" || state == "archived" || status == "gone" || status == "stopped":
		return host.Gone
	case status == "busy" || state == "working" || state == "running":
		if string(a.PID) == "null" || string(a.PID) == "0" || string(a.PID) == `""` {
			return host.Idle
		}
		return host.Busy
	case status == "idle" || state == "done" || state == "ready" || state == "idle":
		return host.Idle
	default:
		return host.UnknownState
	}
}

func valid(s host.Session) bool { return s.Host == hostName && uuid.MatchString(s.ID) }

// Status 仅在成功读取完整清单后才把缺失的会话判为 Gone。
func (d *Driver) Status(ctx context.Context, s host.Session) host.State {
	if !valid(s) {
		return host.UnknownState
	}
	d.mu.Lock()
	active := d.current[s.ID] != nil
	d.mu.Unlock()
	if active {
		return host.Busy
	}
	list, err := d.list(ctx, false)
	if err != nil {
		return host.UnknownState
	}
	if a, ok := list[s.ID]; ok {
		return a.state()
	}
	return host.Gone
}

// Stop 先从完整 UUID 解析后台短号。请求成功后须核实当前轮确已结束。
func (d *Driver) Stop(ctx context.Context, s host.Session, timeout time.Duration) error {
	if !valid(s) {
		return host.Unsupported
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	d.mu.Lock()
	active := d.current[s.ID]
	d.mu.Unlock()
	if active != nil {
		active.cancel()
		select {
		case <-active.done:
			if active.closeErr != nil {
				return active.closeErr
			}
		case <-ctx.Done():
			return fmt.Errorf("%w: 停止续轮: %w", host.Unknown, ctx.Err())
		}
	}
	list, err := d.list(ctx, true)
	if err != nil {
		return fmt.Errorf("%w: 核对停止目标: %w", host.Unknown, err)
	}
	a, ok := list[s.ID]
	if !ok || a.state() == host.Gone {
		return nil
	}
	if !shortID.MatchString(a.ID) {
		return fmt.Errorf("%w: 缺少可信后台短号", host.Unknown)
	}
	out, diag, err := host.Run(ctx, d.program, "", []string{"stop", a.ID}, nil, "", timeout, envPrefixes...)
	if err != nil {
		if strings.Contains(strings.ToLower(diag+out), "no job matching") {
			return nil
		}
		return fmt.Errorf("%w: 请在会话清单核对 %s: %w", host.Unknown, s.ID, err)
	}
	for {
		list, err = d.list(ctx, true)
		if err != nil {
			return fmt.Errorf("%w: 核对停止结果: %w", host.Unknown, err)
		}
		a, ok = list[s.ID]
		if !ok || a.state() == host.Gone || a.state() == host.Idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: 核对会话 %s: %w", host.Unknown, s.ID, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var shortID = regexp.MustCompile(`^[a-fA-F0-9]{6,32}$`)

// Open 在 daemon 无交互终端时提示人工进入持久会话。
func (*Driver) Open(_ context.Context, s host.Session) error {
	if !valid(s) {
		return host.Unsupported
	}
	return fmt.Errorf("%w: 可在终端用 claude --resume %s 查看", host.Unsupported, s.ID)
}

// WorktreePath 允许在仓库的独立任务目录中创建 detached worktree。
func (*Driver) WorktreePath(repo, name string) string { return filepath.Join(repo, ".worktrees", name) }

// Statuses 让整拍共用一份后台清单，清单缺失与查询出错必须区分。
func (d *Driver) Statuses(ctx context.Context, sessions []host.Session) map[host.Session]host.State {
	list, err := d.list(ctx, true)
	result := make(map[host.Session]host.State, len(sessions))
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range sessions {
		switch {
		case !valid(s):
			result[s] = host.UnknownState
		case d.current[s.ID] != nil:
			result[s] = host.Busy
		case err != nil:
			result[s] = host.UnknownState
		default:
			a, ok := list[s.ID]
			if !ok {
				result[s] = host.Gone
			} else if a.WaitingFor != "" || strings.EqualFold(a.Status, "waiting") {
				result[s] = host.WaitingPerson
			} else {
				result[s] = a.state()
			}
		}
	}
	return result
}
