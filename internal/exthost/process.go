package exthost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

const protocolMajor = 1

// Clock 提供退避计时与稳定运行时间，测试可在子进程退出后直接拨动。
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}

type wallClock struct{}

// Now 保留单调时钟读数，校准系统时间不会改变本轮运行的稳定期。
func (wallClock) Now() time.Time { return time.Now() }

// After 让监督循环可以同时等待退避到期与 Stop。
func (wallClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Options 在启动前确定。LogDir 由调用方指定；超时、稳定期及 Clock 的零值使用默认值。
// OnHealth 收到 ok 或 error 和原因，须及时返回且不可在回调内同步等待 Stop。
type Options struct {
	KernelVersion    string
	Config           json.RawMessage
	StateDir         string
	LogDir           string
	HandshakeTimeout time.Duration
	ShutdownTimeout  time.Duration
	StablePeriod     time.Duration
	Clock            Clock
	Handler          Handler
	OnHealth         func(id, state, detail string)
}

// Host 独占一个扩展的生命周期。仅握手成功的连接可接受调用，Stop 后不能复用。
type Host struct {
	ext     Extension
	opts    Options
	life    context.Context
	stop    context.CancelFunc
	done    chan struct{}
	stopErr error
	mu      sync.RWMutex
	ready   *Conn
}

type child struct {
	conn     *Conn
	killTree func() error
	exited   chan struct{}
	waitErr  error
	logDone  chan error
	since    time.Time
}

var errIncompatible = errors.New("扩展拒绝 initialize")

// Start 启动已发现的扩展并等待首次握手，ctx 控制本次启动。返回成功后由 Stop 管理生命周期。
func Start(ctx context.Context, ext Extension, opts Options) (*Host, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.Clock == nil {
		opts.Clock = wallClock{}
	}
	if opts.HandshakeTimeout <= 0 {
		opts.HandshakeTimeout = 5 * time.Second
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 3 * time.Second
	}
	if opts.StablePeriod <= 0 {
		opts.StablePeriod = 5 * time.Minute
	}
	// 重启仍使用首次装载的命令与配置，调用方之后可以复用自己的缓冲。
	ext.Manifest.Exec = slices.Clone(ext.Manifest.Exec)
	opts.Config = bytes.Clone(opts.Config)
	life, stop := context.WithCancel(context.Background())
	h := &Host{ext: ext, opts: opts, life: life, stop: stop, done: make(chan struct{})}
	p, err := h.launch(ctx)
	if err != nil {
		stop()
		h.health("error", err.Error())
		return nil, err
	}
	h.publish(p.conn)
	go h.supervise(p)
	return h, nil
}

func (h *Host) health(state, detail string) {
	if f := h.opts.OnHealth; f != nil {
		f(h.ext.Manifest.ID, state, detail)
	}
}

func (h *Host) publish(c *Conn) {
	h.mu.Lock()
	h.ready = c
	h.mu.Unlock()
	if c != nil {
		h.health("ok", "")
	}
}

func (h *Host) launch(ctx context.Context) (*child, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m := h.ext.Manifest
	if m.Protocol != protocolMajor {
		return nil, fmt.Errorf("%s protocol %d 与内核主版本 %d 不兼容", m.ID, m.Protocol, protocolMajor)
	}
	if !validID(m.ID) || len(m.Exec) == 0 || !filepath.IsLocal(m.Exec[0]) {
		return nil, fmt.Errorf("无效的扩展 id 或 exec[0]")
	}
	path := filepath.Join(h.ext.Dir, m.Exec[0])
	// 在交给 os/exec 之前核对原路径，避免 Windows 按 PATHEXT 补猜后缀。
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("exec %s 须为文件", path)
	}
	log, err := openLog(h.opts.LogDir, m.ID)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, m.Exec[1:]...)
	cmd.Dir = h.ext.Dir
	p, err := startChild(cmd, log, h.opts.Handler)
	if err != nil {
		return nil, fmt.Errorf("启动 %s: %w", m.ID, err)
	}
	initCtx, cancel := context.WithTimeout(ctx, h.opts.HandshakeTimeout)
	defer cancel()
	params := protocol.InitializeParams{Protocol: protocolMajor, KernelVersion: h.opts.KernelVersion, Config: h.opts.Config, StateDir: h.opts.StateDir}
	var result protocol.InitializeResult
	err = p.conn.Call(initCtx, "initialize", params, &result)
	if err == nil {
		switch {
		case !result.OK:
			err = errIncompatible
		case result.Capabilities == nil:
			err = errors.New("initialize 缺少 capabilities")
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s initialize: %w", m.ID, errors.Join(err, p.finish()))
	}
	p.since = h.opts.Clock.Now()
	return p, nil
}

func startChild(cmd *exec.Cmd, log *tailLog, handler Handler) (*child, error) {
	// 父进程自己管理管道：后代可能继承 stderr，Wait 必须能先报告主进程退出。
	var pipes [3]struct{ r, w *os.File }
	for i := range pipes {
		r, w, err := os.Pipe()
		if err != nil {
			for _, p := range pipes[:i] {
				_ = p.r.Close()
				_ = p.w.Close()
			}
			return nil, errors.Join(err, log.file.Close())
		}
		pipes[i].r, pipes[i].w = r, w
	}
	stdin, stdout, stderr := pipes[0], pipes[1], pipes[2]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin.r, stdout.w, stderr.w
	kill, err := startTree(cmd)
	_ = stdin.r.Close()
	_ = stdout.w.Close()
	_ = stderr.w.Close()
	if err != nil {
		return nil, errors.Join(err, stdin.w.Close(), stdout.r.Close(), stderr.r.Close(), log.file.Close())
	}
	p := &child{
		conn:     NewConn(stdout.r, stdin.w, handler, 0),
		killTree: kill,
		exited:   make(chan struct{}),
		logDone:  make(chan error, 1),
	}
	go func() { p.waitErr = cmd.Wait(); close(p.exited) }()
	go func() { p.logDone <- log.capture(stderr.r) }()
	return p, nil
}

func (p *child) finish() error {
	// 即使主进程已经退出，后代仍可能持有标准流，先清理整棵树再等日志读完。
	killErr := p.killTree()
	connErr := p.conn.Close()
	<-p.exited
	return errors.Join(killErr, connErr, <-p.logDone)
}

func (h *Host) supervise(p *child) {
	defer close(h.done)
	defer h.stop()
	delays := [...]time.Duration{2 * time.Second, 10 * time.Second, time.Minute, 5 * time.Minute}
	attempt := 0
	for {
		if p != nil {
			var reason error
			select {
			case <-h.life.Done():
				h.publish(nil)
				h.stopErr = h.shutdown(p)
				return
			case <-p.exited:
				// 即使后代仍握有 stdout，也要处理主进程的退出。
			case <-p.conn.ctx.Done():
				reason = p.conn.failure()
			}
			h.publish(nil)
			stable := h.opts.Clock.Now().Sub(p.since) >= h.opts.StablePeriod
			err := p.finish()
			if h.life.Err() != nil {
				h.stopErr = err
				return
			}
			reason = errors.Join(reason, p.waitErr, err)
			if reason == nil {
				reason = errors.New("扩展提前退出")
			}
			h.health("error", reason.Error())
			if stable {
				attempt = 0
			}
		}
		select {
		case <-h.life.Done():
			return
		case <-h.opts.Clock.After(delays[attempt]):
		}
		if h.life.Err() != nil {
			return
		}
		attempt = min(attempt+1, len(delays)-1)
		var err error
		p, err = h.launch(h.life)
		if err != nil {
			if h.life.Err() != nil {
				return
			}
			h.health("error", err.Error())
			if errors.Is(err, errIncompatible) {
				return
			}
			continue
		}
		if h.life.Err() != nil {
			h.stopErr = h.shutdown(p)
			return
		}
		h.publish(p.conn)
	}
}

func (h *Host) shutdown(p *child) error {
	ctx, cancel := context.WithTimeout(context.Background(), h.opts.ShutdownTimeout)
	defer cancel()
	// 一些扩展直接退出，一些先回响应再清理；两段共用同一个截止时刻。
	err := p.conn.Call(ctx, "shutdown", struct{}{}, nil)
	select {
	case <-p.exited:
	case <-ctx.Done():
	}
	if errors.Is(err, ErrUnavailable) || errors.Is(err, context.DeadlineExceeded) {
		err = nil
	}
	return errors.Join(err, p.finish())
}

// Stop 等待优雅关闭或超时终止、子进程回收及日志排空，可由多个调用方同时等待。
func (h *Host) Stop() error {
	h.stop()
	<-h.done
	return h.stopErr
}

// Call 在当前已握手的连接上传递请求。扩展自定义端口可使用自己的参数和结果类型。
// 退避期间立即返回 ErrUnavailable，避免在新的扩展进程里隐式重放旧请求。
func (h *Host) Call(ctx context.Context, method string, params, result any) error {
	c := h.connection()
	if c == nil {
		return ErrUnavailable
	}
	return c.Call(ctx, method, params, result)
}

func (h *Host) connection() *Conn {
	h.mu.RLock()
	c := h.ready
	h.mu.RUnlock()
	if h.life.Err() != nil {
		return nil
	}
	return c
}

// AlarmRun 将原定触发时刻转发给扩展，调用方通过 ctx 控制超时。
func (h *Host) AlarmRun(ctx context.Context, alarmID, reason string, scheduledAtMs int64) error {
	c := h.connection()
	if c == nil {
		return ErrUnavailable
	}
	return c.CallWaiting(ctx, "alarm/run", protocol.AlarmRunParams{AlarmID: alarmID, Reason: reason, ScheduledAtMs: scheduledAtMs}, nil)
}

// ActionExecute 转发预览或执行请求；返回体的业务字段由动作定义。
func (h *Host) ActionExecute(ctx context.Context, params protocol.ActionExecuteParams) (json.RawMessage, error) {
	return h.rawCall(ctx, "action/execute", params)
}

// ActionVerify 请求扩展核实与幂等键对应的动作结果。
func (h *Host) ActionVerify(ctx context.Context, params protocol.ActionVerifyParams) (json.RawMessage, error) {
	return h.rawCall(ctx, "action/verify", params)
}

// ContextRead 向扩展读取指定对象的一页上下文。
func (h *Host) ContextRead(ctx context.Context, params protocol.ContextReadParams) (json.RawMessage, error) {
	return h.rawCall(ctx, "context/read", params)
}

func (h *Host) rawCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	var result json.RawMessage
	err := h.Call(ctx, method, params, &result)
	return result, err
}
