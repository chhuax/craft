package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

const tokenVar = "CRAFT_TEST_GITHUB_TOKEN"
const secret = "ghp_replay_only_194267"

var instant = time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

type harness struct {
	t      *testing.T
	wire   net.Conn
	reader *bufio.Reader
	clock  atomic.Int64
	seq    int
	calls  []frame
	state  map[string]json.RawMessage
	subs   []subscription
	reject string
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func raw(t testing.TB, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	must(t, err)
	return b
}
func start(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	t.Setenv(tokenVar, secret)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	h, k := net.Pipe()
	x := &harness{t: t, wire: h, reader: bufio.NewReader(h), state: map[string]json.RawMessage{}}
	x.clock.Store(instant.UnixMilli())
	done := make(chan error, 1)
	go func() {
		defer func() { _ = k.Close() }()
		done <- serve(k, k, func() time.Time { return time.UnixMilli(x.clock.Load()).UTC() })
	}()
	// 截止时间只用于报告死锁，断言先后依赖管道消息和显式信号。
	must(t, h.SetDeadline(time.Now().Add(30*time.Second)))
	t.Cleanup(func() { _ = h.Close(); must(t, <-done) })
	cfg := map[string]any{"apiBase": srv.URL, "tokenEnv": tokenVar, "pollWindowDays": 7}
	params := map[string]any{"config": cfg, "protocol": 2, "stateDir": t.TempDir(), "futureField": true}
	if _, err := x.invoke("initialize", params); err == nil {
		t.Fatal("接受了错误主版本")
	}
	params["protocol"] = 1
	var init protocol.InitializeResult
	must(t, json.Unmarshal(x.ok("initialize", params), &init))
	if !init.OK || init.Capabilities == nil {
		t.Fatal(init)
	}
	return x
}
func (h *harness) send(f frame) {
	h.t.Helper()
	f.JSONRPC = "2.0"
	_, err := h.wire.Write(append(raw(h.t, f), '\n'))
	must(h.t, err)
}
func (h *harness) invoke(method string, p any) (json.RawMessage, error) {
	h.t.Helper()
	h.seq++
	id := raw(h.t, h.seq)
	h.send(frame{Method: method, ID: id, Params: raw(h.t, p)})
	for {
		line, err := h.reader.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		if strings.Contains(string(line), secret) {
			h.t.Fatal("协议输出泄露令牌")
		}
		var f frame
		must(h.t, json.Unmarshal(line, &f))
		if f.Method == "" {
			if string(f.ID) != string(id) {
				h.t.Fatal("响应标识不匹配")
			}
			if f.Error != nil {
				return nil, f.Error
			}
			return f.Result, nil
		}
		h.calls = append(h.calls, f)
		if f.Method == h.reject {
			h.send(frame{ID: f.ID, Error: &protocol.Error{Code: -32603, Message: "重放拒绝"}})
			continue
		}
		var result any
		var kv struct {
			Key   string
			Value json.RawMessage
		}
		switch f.Method {
		case "state/get":
			must(h.t, json.Unmarshal(f.Params, &kv))
			result = h.state[kv.Key]
		case "state/set":
			must(h.t, json.Unmarshal(f.Params, &kv))
			h.state[kv.Key] = kv.Value
		case "thread/subscribed":
			result = h.subs
		case "watch/receive":
			result = "queued"
		case "health/report", "thread/describe", "thread/closed", "thread/reopened", "work/bindThread":
		default:
			h.t.Fatalf("未知内核调用 %s", f.Method)
		}
		h.send(frame{ID: f.ID, Result: raw(h.t, result)})
	}
}
func (h *harness) ok(method string, p any) json.RawMessage {
	h.t.Helper()
	v, err := h.invoke(method, p)
	must(h.t, err)
	return v
}
func (h *harness) run(id string) error {
	h.calls = nil
	_, err := h.invoke("alarm/run", protocol.AlarmRunParams{AlarmID: id, Reason: "requested", ScheduledAtMs: h.clock.Load()})
	return err
}
func (h *harness) methods() []string {
	var methods []string
	for _, c := range h.calls {
		methods = append(methods, c.Method)
	}
	return methods
}
func (h *harness) triggers() []protocol.Trigger {
	var ts []protocol.Trigger
	for _, c := range h.calls {
		if c.Method == "watch/receive" {
			var tr protocol.Trigger
			must(h.t, json.Unmarshal(c.Params, &tr))
			ts = append(ts, tr)
		}
	}
	return ts
}
func (h *harness) health(state string) bool {
	for _, c := range h.calls {
		if c.Method == "health/report" {
			var p struct{ State string }
			must(h.t, json.Unmarshal(c.Params, &p))
			if p.State == state {
				return true
			}
		}
	}
	return false
}

func TestCancelAlarm(t *testing.T) {
	entered := make(chan struct{})
	canceled := make(chan struct{})
	h := start(t, func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(canceled) })
	// 读循环由调用方继续处理。另一端只在 HTTP 请求进入后发取消，避免靠调度猜测。
	var wg sync.WaitGroup
	id := raw(t, h.seq+1)
	wg.Go(func() {
		select {
		case <-entered:
		case <-t.Context().Done():
			return
		}
		h.send(frame{Method: "$/cancel", Params: raw(t, map[string]any{"id": id})})
	})
	err := h.run("inbox")
	wg.Wait()
	rpc, ok := errors.AsType[*protocol.Error](err)
	if !ok || rpc.Code != -32800 || slices.Contains(h.methods(), "state/set") {
		t.Fatalf("取消结果 %v %v", err, h.methods())
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP 请求未取消")
	}
	h.ok("shutdown", struct{}{})
}

func TestNetworkFailure(t *testing.T) {
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	})
	if h.run("inbox") == nil || !h.health("error") {
		t.Fatal("网络错误未进入退避")
	}
}

func BenchmarkNotifications1000(b *testing.B) {
	b.Setenv(tokenVar, secret)
	pages := make([][]byte, 10)
	for i := range pages {
		rows := make([]map[string]any, 100)
		for j := range rows {
			rows[j] = notice("mention", "Issue", i*100+j+1)
		}
		pages[i] = raw(b, rows)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/notifications":
			page := pageNumber(r)
			if page < 10 {
				nextLink(w, page+1)
			}
			_, _ = w.Write(pages[page-1])
		case "/user":
			_, _ = w.Write([]byte(`{"login":"reader","type":"User"}`))
		default:
			writeComment(w, r)
		}
	}))
	defer srv.Close()
	p := protocol.InitializeParams{Protocol: 1, Config: raw(b, map[string]string{"apiBase": srv.URL, "tokenEnv": tokenVar}), StateDir: b.TempDir()}
	e, err := initialize(raw(b, p), func(_ context.Context, method string, _, dst any) error {
		if method == "watch/receive" {
			*dst.(*string) = "queued"
		}
		return nil
	}, func() time.Time { return instant })
	must(b, err)
	b.ReportAllocs()
	for b.Loop() {
		must(b, e.inbox(b.Context()))
	}
}

func TestMalformedInputLog(t *testing.T) {
	t.Setenv(tokenVar, secret)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "--github-stdio")
	cmd.Dir = t.TempDir()
	cmd.Stdin = strings.NewReader("{" + secret + "\n")
	output, err := cmd.CombinedOutput()
	if err == nil || len(output) == 0 || strings.Contains(string(output), secret) {
		t.Fatal("无效输入的进程诊断泄露凭据或未报错")
	}
}

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--github-stdio" {
		main()
		os.Exit(0)
	}

	os.Exit(m.Run())
}
