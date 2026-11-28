// Package codex 使用 daemon 持有的 stdio app-server 管理会话；不同实例之间不争夺写入权。
package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/host"
)

const frameLimit = 4 << 20

// app-server 可以省略信封的固定版本字段。只在该传输入口补齐，扩展协议仍严格校验。
// 直接复用行的原始字节保留大整数 ID 与载荷；行上限在补齐前即生效。
type frames struct {
	file  *os.File
	lines *bufio.Scanner
	tail  []byte
}

func newFrames(f *os.File) *frames {
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), frameLimit)
	return &frames{file: f, lines: s}
}

func (r *frames) Read(b []byte) (int, error) {
	if len(r.tail) == 0 {
		if !r.lines.Scan() {
			if err := r.lines.Err(); err != nil {
				return 0, err
			}
			return 0, io.EOF
		}
		line := bytes.TrimSpace(r.lines.Bytes())
		var hdr struct {
			Version json.RawMessage `json:"jsonrpc"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil || len(line) == 0 || line[0] != '{' {
			return 0, errors.New("app-server 帧无效")
		}
		if hdr.Version == nil && len(line) > 2 {
			r.tail = append(r.tail[:0], `{"jsonrpc":"2.0",`...)
			line = line[1:]
		}
		r.tail = append(r.tail, line...)
		r.tail = append(r.tail, '\n')
	}
	n := copy(b, r.tail)
	r.tail = r.tail[n:]
	return n, nil
}

func (r *frames) Close() error { return r.file.Close() }

type server struct {
	program string
	life    context.Context
	cancel  context.CancelFunc
	now     func() time.Time

	// 只串行启停和握手。Conn 自行关联并发请求与响应；丢线时不得把旧写请求搬到新连接上。
	mu         sync.Mutex
	current    *connection
	retryAt    time.Time
	failures   int
	watch      sync.WaitGroup
	cleanupErr error
	gate       chan struct{}
}
type connection struct {
	proc    *host.Process
	rpc     *exthost.Conn
	started time.Time
}

func newServer(program string) *server {
	ctx, cancel := context.WithCancel(context.Background())
	return &server{program: program, life: ctx, cancel: cancel, now: time.Now, gate: make(chan struct{}, 1)}
}

func (s *server) connect(ctx context.Context) (*connection, error) {
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.life.Done():
		return nil, s.life.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.life.Err() != nil {
		return nil, errors.New("app-server 已关闭")
	}
	if c := s.current; c != nil {
		select {
		case <-c.rpc.Done():
			s.dropLocked(c)
		default:
			return c, nil
		}
	}
	if s.now().Before(s.retryAt) {
		return nil, fmt.Errorf("%w: app-server 重连退避中", host.Later)
	}
	// 启动等待也受本次请求控制；握手成功后交由 daemon 的生命周期接管。
	life, cancel := context.WithCancel(context.Background())
	stopClose := context.AfterFunc(s.life, cancel)
	defer stopClose()
	stopCancel := context.AfterFunc(ctx, cancel)
	defer stopCancel()
	p, err := host.Start(life, s.program, "", []string{"app-server"}, host.BindingEnv(host.Session{Host: hostName}, nil), "OPENAI_", "CODEX_")
	if err != nil {
		cancel()
		if !errors.Is(err, host.Unsupported) {
			s.backoff()
		}
		return nil, err
	}
	c := &connection{proc: p, rpc: exthost.NewConn(newFrames(p.Stdout), p.Stdin, nil, frameLimit+32), started: s.now()}
	params := map[string]any{"clientInfo": map[string]string{"name": "craft", "version": "1"}}
	var reply struct {
		UserAgent string `json:"userAgent"`
	}
	err = c.rpc.Call(ctx, "initialize", params, &reply)
	if err == nil && reply.UserAgent == "" {
		err = errors.New("initialize 缺少 userAgent")
	}
	if err == nil {
		err = c.rpc.Notify(ctx, "initialized", nil)
	}
	if err != nil || !stopCancel() || !stopClose() || ctx.Err() != nil || s.life.Err() != nil {
		cancel()
		s.backoff()
		return nil, fmt.Errorf("app-server 握手: %w", errors.Join(err, ctx.Err(), s.life.Err(), c.rpc.Close(), p.Close()))
	}
	s.current = c
	s.watch.Go(func() {
		<-c.rpc.Done()
		// 正常关闭时，先让持锁的关闭方等进程处理完 EOF。断线清理只作用于本代。
		defer cancel()
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.current == c {
			s.dropLocked(c)
		}
	})
	return c, nil
}

func (s *server) backoff() {
	delays := [...]time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, time.Minute}
	s.retryAt = s.now().Add(delays[s.failures])
	s.failures = min(s.failures+1, len(delays)-1)
}

func (s *server) dropLocked(c *connection) {
	s.current = nil
	if s.now().Sub(c.started) >= time.Minute {
		s.failures = 0
	}
	s.backoff()
	s.cleanupErr = errors.Join(s.cleanupErr, c.rpc.Close(), c.proc.Close())
}

// close 先禁止新连接，再结束 stdio 及其进程树，最后等本代读写与断线观察者退出。
func (s *server) close() error {
	s.cancel()
	s.mu.Lock()
	if c := s.current; c != nil {
		s.current = nil
		// EOF 是 stdio 服务的正常退出信号。超时后再关闭读取端并终止整棵树。
		_ = c.proc.Stdin.Close()
		select {
		case <-c.proc.Done():
		case <-time.After(2 * time.Second):
		}
		s.cleanupErr = errors.Join(s.cleanupErr, c.rpc.Close(), c.proc.Close())
	}
	err := s.cleanupErr
	s.mu.Unlock()
	s.watch.Wait()
	return err
}

func (s *server) call(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := s.connect(ctx)
	if errors.Is(err, host.Unsupported) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%s 发送前: %w", method, errors.Join(host.Later, err))
	}
	err = c.rpc.Call(ctx, method, params, result)
	if err != nil && (ctx.Err() != nil || errors.Is(err, exthost.ErrUnavailable)) {
		s.mu.Lock()
		if s.current == c {
			s.dropLocked(c)
		}
		s.mu.Unlock()
	}
	return err
}
