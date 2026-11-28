package exthost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

type wirePeer struct {
	*Conn
	to   *io.PipeWriter
	from *bufio.Reader
}

func wire(t *testing.T, handler Handler, limit int) wirePeer {
	t.Helper()
	r, to := io.Pipe()
	from, w := io.Pipe()
	c := NewConn(r, w, handler, limit)
	t.Cleanup(func() { _ = c.Close(); _ = from.Close(); _ = to.Close() })
	return wirePeer{c, to, bufio.NewReader(from)}
}
func (p wirePeer) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.to, s+"\n"); err != nil {
		t.Fatal(err)
	}
}
func (p wirePeer) read(t *testing.T, dest any) {
	t.Helper()
	b, err := p.from.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, dest); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentCalls(t *testing.T) {
	p := wire(t, nil, 0)
	const n = 64
	errs := make(chan error, n)
	for i := range n {
		go func() {
			var got int
			err := p.Call(t.Context(), "echo", []int{i}, &got)
			if err == nil && got != i {
				err = fmt.Errorf("响应 %d 给了 %d", got, i)
			}
			errs <- err
		}()
	}
	var requests []protocol.Request
	ids := map[string]bool{}
	for range n {
		var r protocol.Request
		p.read(t, &r)
		ids[string(r.ID)] = true
		requests = append(requests, r)
	}
	for i := n - 1; i >= 0; i-- {
		r := requests[i]
		var args []int
		if err := json.Unmarshal(*r.Params, &args); err != nil {
			t.Fatal(err)
		}
		p.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%d}`, r.ID, args[0]))
	}
	for i := range n {
		if err := <-errs; err != nil || !ids[fmt.Sprint(i+1)] {
			t.Fatal(i, err)
		}
	}
}

func TestCancelCallWhileWriteBlocked(t *testing.T) {
	p := wire(t, nil, 0)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.Call(ctx, "block", nil, nil) }()
	if _, err := p.from.Peek(1); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消等待了写入")
	}
	var r protocol.Request
	p.read(t, &r)
	var notice protocol.Notification
	p.read(t, &notice)
	if notice.Method != "$/cancel" || string(*notice.Params) != fmt.Sprintf(`{"id":%s}`, r.ID) {
		t.Fatal(notice)
	}
}

func TestInbound(t *testing.T) {
	canceled := make(chan struct{})
	notice := make(chan struct{})
	started := make(chan struct{})
	p := wire(t, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		switch method {
		case "block":
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		case "event":
			close(notice)
			return nil, nil
		case "echo":
			var a []int
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("%w: %v", ErrInvalidParams, err)
			}
			return a, nil
		default:
			return nil, ErrMethodNotFound
		}
	}, 0)
	p.send(t, `{"jsonrpc":"2.0","id":"slow","method":"block"}`)
	p.send(t, `{"jsonrpc":"2.0","method":"event"}`)
	<-notice
	// 每个请求在自己的 goroutine 里处理：收到 event 并不代表 block 已经开始运行。
	// 若取消先于它开始，连接按设计跳过已取消请求的处理函数，close(canceled) 就永远不会执行。
	<-started
	p.send(t, `{"jsonrpc":"2.0","method":"$/cancel","params":{"id":"slow"}}`)
	<-canceled
	var r protocol.Response
	p.read(t, &r)
	if string(r.ID) != `"slow"` {
		t.Fatal(r)
	}
	for _, tc := range []struct {
		id, method, params string
		code               int
	}{{`"a"`, "missing", "{}", -32601}, {"9007199254740993", "echo", "{}", -32602}, {"4", "echo", "3", -32602}, {`"b"`, "echo", "[9]", 0}} {
		p.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":%q,"params":%s,"future":true}`, tc.id, tc.method, tc.params))
		r = protocol.Response{}
		p.read(t, &r)
		if string(r.ID) != tc.id || (tc.code != 0 && (r.Error == nil || r.Error.Code != tc.code)) || (tc.code == 0 && string(r.Result) != "[9]") {
			t.Fatal(r)
		}
	}
}

func TestStrictLines(t *testing.T) {
	p := wire(t, nil, 0)
	for _, line := range []string{`{"jsonrpc":"2.0","id":1,"result":NaN}`, `{"v":Infinity}`, "{} {}", "{", `{"method":"` + "\xff" + `"}`} {
		p.send(t, line)
		var r protocol.Response
		p.read(t, &r)
		if r.Error == nil || r.Error.Code != -32700 {
			t.Fatal(line, r)
		}
	}
	for _, tc := range []struct{ name, line string }{{"过长", strings.Repeat(" ", 17000)}, {"未结束", `{"jsonrpc":"2.0","method":"echo"}`}} {
		t.Run(tc.name, func(t *testing.T) {
			p := wire(t, nil, 16384)
			done := make(chan error, 1)
			go func() { done <- p.Call(t.Context(), "open", nil, nil) }()
			var r protocol.Request
			p.read(t, &r)
			_, _ = io.WriteString(p.to, tc.line)
			_ = p.to.Close()
			if err := <-done; !errors.Is(err, ErrUnavailable) || !ErrUnavailable.Retryable() {
				t.Fatal(err)
			}
		})
	}
}

func TestBoundedHandlersAndClose(t *testing.T) {
	start := runtime.NumGoroutine()
	entered := make(chan struct{}, 100)
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	defer release()
	p := wire(t, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
		case <-released:
		}
		return nil, nil
	}, 0)
	for i := range 100 {
		p.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","method":"hold","id":%d}`, i+1))
	}
	for range 64 {
		<-entered
	}
	if extra := runtime.NumGoroutine() - start; extra > 70 {
		t.Fatalf("100 个请求启动了 %d 个 goroutine", extra)
	}
	// 队列饱和时读循环仍须能处理对向响应以及正在执行的请求的取消。
	p.send(t, `{"jsonrpc":"2.0","method":"$/cancel","params":{"id":1}}`)
	var res protocol.Response
	p.read(t, &res)
	if string(res.ID) != "1" {
		t.Fatal(res)
	}
	<-entered
	const n = 16
	done := make(chan error, n)
	for range n {
		go func() { done <- p.Call(t.Context(), "hold", nil, nil) }()
	}
	for range n {
		var req protocol.Request
		p.read(t, &req)
	}
	if err := p.to.Close(); err != nil {
		t.Fatal(err)
	}
	for range n {
		if err := <-done; !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	}
	release()
	for range 2 {
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for runtime.NumGoroutine() > start+1 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if n := runtime.NumGoroutine(); n > start+1 {
		t.Fatalf("关闭后 goroutine 数 %d，启动前 %d", n, start)
	}
}

func TestRPCLineLimitBeforeNewline(t *testing.T) {
	p := wire(t, nil, 32)
	done := make(chan error, 1)
	go func() { done <- p.Call(t.Context(), "open", nil, nil) }()
	var req protocol.Request
	p.read(t, &req)
	_, err := io.WriteString(p.to, strings.Repeat(" ", 33))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("超限后仍在等待换行")
	}
}

func TestRawIDAndRemoteError(t *testing.T) {
	id := `"x<\u0061"`
	p := wire(t, func(context.Context, string, json.RawMessage) (any, error) {
		return nil, &protocol.Error{Code: 27, Message: "blocked"}
	}, 0)
	p.send(t, `{"jsonrpc":"2.0","method":"read","id":`+id+"}")
	var response protocol.Response
	p.read(t, &response)
	if string(response.ID) != id || response.Error == nil || response.Error.Code != 27 {
		t.Fatalf("%s %+v", response.ID, response.Error)
	}
}

func TestCloseInterruptsBlockedWrite(t *testing.T) {
	r, to := io.Pipe()
	from, w := io.Pipe()
	c := NewConn(r, w, nil, 0)
	t.Cleanup(func() { _ = c.Close(); _ = to.Close(); _ = from.Close() })
	done := make(chan error, 1)
	go func() { done <- c.Call(t.Context(), "pending", nil, nil) }()
	var first [1]byte
	if _, err := io.ReadFull(from, first[:]); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestFragmentedLineAtLimit(t *testing.T) {
	const limit = 8192
	p := wire(t, nil, limit)
	line := `{"jsonrpc":"2.0","id":"ok","method":"unknown"}`
	for _, chunk := range []string{line[:5], line[5:] + strings.Repeat(" ", limit-len(line)), "\n"} {
		if _, err := io.WriteString(p.to, chunk); err != nil {
			t.Fatal(err)
		}
	}
	var response protocol.Response
	p.read(t, &response)
	if string(response.ID) != `"ok"` || response.Error == nil || response.Error.Code != -32601 {
		t.Fatal(response)
	}
}

type closeFailure struct {
	*io.PipeWriter
	failure error
}

func (w closeFailure) Close() error { return errors.Join(w.PipeWriter.Close(), w.failure) }

func TestCloseErrorIsRepeatable(t *testing.T) {
	r, to := io.Pipe()
	from, w := io.Pipe()
	cause := errors.New("写端关闭失败")
	c := NewConn(r, closeFailure{w, cause}, nil, 0)
	defer func() { _ = to.Close(); _ = from.Close() }()
	for range 2 {
		if err := c.Close(); !errors.Is(err, cause) {
			t.Fatal(err)
		}
	}
}

func TestHandlerErrorWithInvalidData(t *testing.T) {
	p := wire(t, func(context.Context, string, json.RawMessage) (any, error) {
		bad := json.RawMessage("NaN")
		return nil, &protocol.Error{Code: 8, Message: "bad", Data: &bad}
	}, 0)
	p.send(t, `{"jsonrpc":"2.0","id":"s","method":"read"}`)
	response := make(chan protocol.Response, 1)
	go func() {
		var r protocol.Response
		line, err := p.from.ReadBytes('\n')
		if err == nil {
			_ = json.Unmarshal(line, &r)
		}
		response <- r
	}()
	select {
	case r := <-response:
		if r.Error == nil || r.Error.Code != -32603 || string(r.ID) != `"s"` {
			t.Fatal(r)
		}
	case <-time.After(time.Second):
		t.Fatal("错误详情编码失败后丢失了响应")
	}
}

func TestAlarmRunWaitsForRemote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := wire(t, nil, 0)
		h := Host{ready: p.Conn, life: t.Context()}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.AlarmRun(ctx, "poll", "schedule", 42) }()
		var request, notification protocol.Request
		p.read(t, &request)
		<-ctx.Done()
		p.read(t, &notification)
		if notification.Method != "$/cancel" {
			t.Fatal(notification)
		}
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("远端尚未结束: %v", err)
		default:
		}
		p.send(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, request.ID))
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	})
}
