package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/host"
)

type stream struct {
	p        *host.Process
	cancel   context.CancelFunc
	done     chan struct{}
	closeErr error

	// stdin 帧串行写入。读端单独关联重放的 user 消息，不能按最终 result 的个数推断投递次数。
	writeMu sync.Mutex
	mu      sync.Mutex
	ended   bool
	count   int
	pending map[string]chan host.Result
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type userFrame struct {
	Type    string  `json:"type"`
	Message message `json:"message"`
}

// Append 只向自己仍持有的 stdin 追加运行中输入。其他 busy 或等待审批的会话返回 Later。
func (d *Driver) Append(ctx context.Context, s host.Session, text, id string) host.Result {
	marked, err := host.MarkDelivery(id, text)
	if err != nil || !valid(s) {
		return host.Result{Outcome: host.Unsupported, Err: host.Unsupported}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	active, result := d.forAppend(ctx, s)
	if active == nil {
		return result
	}
	return active.send(ctx, marked, id)
}

func (d *Driver) forAppend(ctx context.Context, s host.Session) (*stream, host.Result) {
	d.mu.Lock()
	if err := d.life.Err(); err != nil {
		d.mu.Unlock()
		return nil, host.Result{Outcome: host.Later, Err: err}
	}
	if a := d.current[s.ID]; a != nil {
		starting := a.p == nil
		d.mu.Unlock()
		if starting {
			return nil, host.Result{Outcome: host.Later}
		}
		return a, host.Result{}
	}
	// 先占这个会话的启动位置。读取派工信息与启动进程均在锁外，停止可以取消正在启动的轮次。
	life, cancel := context.WithCancel(d.life)
	a := &stream{cancel: cancel, done: make(chan struct{}), pending: map[string]chan host.Result{}}
	d.current[s.ID] = a
	delete(d.replies, s.ID)
	d.workers.Add(1)
	d.mu.Unlock()
	startCtx, stop := context.WithCancel(ctx)
	stopLife := context.AfterFunc(life, stop)
	defer stopLife()
	defer stop()
	p, r := d.resume(startCtx, life, s, cancel)
	if p == nil {
		cancel()
		d.mu.Lock()
		delete(d.current, s.ID)
		d.mu.Unlock()
		close(a.done)
		d.workers.Done()
		return nil, r
	}
	d.mu.Lock()
	a.p = p
	d.mu.Unlock()
	go func() { defer d.workers.Done(); d.read(s, a) }()
	return a, host.Result{}
}

func (d *Driver) resume(ctx, life context.Context, s host.Session, cancel context.CancelFunc) (*host.Process, host.Result) {
	list, err := d.list(ctx, true)
	if err != nil {
		return nil, host.Result{Outcome: host.Later, Err: err}
	}
	a, ok := list[s.ID]
	if !ok || a.state() == host.Gone {
		return nil, host.Result{Outcome: host.Unknown, Err: fmt.Errorf("请核对会话 %s 是否存在", s.ID)}
	}
	if a.state() != host.Idle {
		return nil, host.Result{Outcome: host.Later}
	}
	var spec host.CreateSpec
	if d.lookup != nil {
		spec, err = d.lookup(ctx, s)
		if err != nil {
			return nil, host.Result{Outcome: host.Later, Err: fmt.Errorf("读取当前派工: %w", err)}
		}
	} else {
		d.mu.Lock()
		spec, ok = d.specs[s.ID]
		d.mu.Unlock()
		if !ok {
			return nil, host.Result{Outcome: host.Unsupported, Err: fmt.Errorf("%w: 缺少当前派工的权限与目录", host.Unsupported)}
		}
	}
	args, err := permissionArgs(spec.Permissions)
	if err != nil {
		return nil, host.Result{Outcome: host.Unsupported, Err: err}
	}
	args = append([]string{"--resume", s.ID}, args...)
	args = append(args, "--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages", "--verbose", "-p")
	stop := context.AfterFunc(ctx, cancel)
	p, err := host.Start(life, d.program, spec.Directory, args, host.BindingEnv(s, spec.Env), envPrefixes...)
	stop()
	if err != nil {
		cancel()
		return nil, host.Result{Outcome: host.Later, Err: err}
	}
	return p, host.Result{}
}

func (a *stream) send(ctx context.Context, marked, id string) host.Result {
	a.writeMu.Lock()
	if err := ctx.Err(); err != nil {
		a.writeMu.Unlock()
		return host.Result{Outcome: host.Later, Err: err}
	}
	a.mu.Lock()
	if a.ended || a.pending[marked] != nil {
		a.mu.Unlock()
		a.writeMu.Unlock()
		return host.Result{Outcome: host.Later}
	}
	reply := make(chan host.Result, 1)
	a.count++
	a.pending[marked] = reply
	a.mu.Unlock()
	stop := context.AfterFunc(ctx, a.cancel)
	err := json.NewEncoder(a.p.Stdin).Encode(userFrame{"user", message{"user", marked}})
	a.writeMu.Unlock()
	defer stop()
	if err == nil {
		select {
		case r := <-reply:
			if r.Outcome == host.OK {
				return r
			}
			err = r.Err
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	a.cancel()
	return host.Result{Outcome: host.Unknown, Err: fmt.Errorf("请在会话历史核对投递标记 %s: %w", id, err)}
}

func (a *stream) accept(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if reply := a.pending[text]; reply != nil {
		reply <- host.Result{Outcome: host.OK}
		delete(a.pending, text)
	}
}

func replayText(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "")
}

func (d *Driver) read(s host.Session, a *stream) {
	scan := bufio.NewScanner(a.p.Stdout)
	scan.Buffer(make([]byte, 4096), 4<<20)
	var result string
	var confirmed bool
	for scan.Scan() {
		var event struct {
			Type, Subtype string
			Result        *string
			Session       string `json:"session_id"`
			IsError       *bool  `json:"is_error"`
			Message       struct {
				Role    string
				Content json.RawMessage
			}
		}
		if json.Unmarshal(scan.Bytes(), &event) != nil || event.Session != "" && event.Session != s.ID {
			break
		}
		switch event.Type {
		case "user":
			if event.Message.Role == "user" {
				a.accept(replayText(event.Message.Content))
			}
		case "result":
			confirmed = event.Result != nil && event.Session == s.ID &&
				(event.IsError != nil && !*event.IsError || event.IsError == nil && event.Subtype == "success")
			if confirmed {
				result = *event.Result
			}
		default:
			continue
		}
		if event.Type == "result" {
			break
		}
	}
	a.mu.Lock()
	a.ended = true
	for _, ch := range a.pending {
		// 唯一输入可以由成功的最终结果确认。多条输入共用一个结果时，仅认各自的重放回执。
		if confirmed && a.count == 1 {
			ch <- host.Result{Outcome: host.OK}
		} else {
			ch <- host.Result{Outcome: host.Unknown, Err: errors.New("缺少独立输入回执")}
		}
	}
	a.mu.Unlock()
	_ = a.p.Stdin.Close()
	if confirmed {
		// 给 CLI 留出落盘和退出的时间，最终 result 可能先于持久化完成。
		select {
		case <-a.p.Done():
		case <-time.After(2 * time.Second):
		}
	}
	a.closeErr = a.p.Close()
	a.cancel()
	d.mu.Lock()
	delete(d.current, s.ID)
	if confirmed {
		d.replies[s.ID] = result
	}
	d.mu.Unlock()
	close(a.done)
}

// LastReply 返回此实例实际收到的最后一个成功 result。缺少回执时由上层提示人查看宿主。
func (d *Driver) LastReply(_ context.Context, s host.Session) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if text, ok := d.replies[s.ID]; ok && valid(s) {
		return text, nil
	}
	return "", host.Unsupported
}

// Close 结束本实例持有的流式命令，并等候最后的读取与进程树回收完成。
func (d *Driver) Close() error {
	d.cancel()
	d.mu.Lock()
	active := make([]*stream, 0, len(d.current))
	for _, a := range d.current {
		active = append(active, a)
	}
	d.mu.Unlock()
	d.workers.Wait()
	var err error
	for _, a := range active {
		err = errors.Join(err, a.closeErr)
	}
	return err
}
