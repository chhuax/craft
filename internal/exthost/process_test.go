package exthost

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

const fixtureEnv = "CRAFT_TEST_EXTENSION"

func TestMain(m *testing.M) {
	if mode := os.Getenv(fixtureEnv); mode != "" {
		if err := fakeExtension(mode); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeExtension(mode string) error {
	if len(os.Args) > 1 && os.Args[1] == "child" {
		for {
			time.Sleep(time.Hour)
		}
	}
	input := bufio.NewScanner(os.Stdin)
	output := json.NewEncoder(os.Stdout)
	var alarmID json.RawMessage
	for input.Scan() {
		var req protocol.Request
		if err := json.Unmarshal(input.Bytes(), &req); err != nil {
			return err
		}
		result := json.RawMessage(`{}`)
		switch req.Method {
		case "initialize":
			if mode == "timeout" {
				continue
			}
			if mode == "invalid" {
				_, err := fmt.Fprintln(os.Stdout, "{")
				return err
			}
			if mode == "long" {
				_, err := fmt.Fprintln(os.Stdout, strings.Repeat("x", 17<<20))
				return err
			}
			var err error
			result, err = fakeInitialize(mode, *req.Params)
			if err != nil {
				return err
			}
		case "fixture/environment":
			_, leaked := os.LookupEnv("ANTHROPIC_API_KEY")
			_, missing := os.LookupEnv("ABSENT_TOKEN")
			var err error
			result, err = json.Marshal([]bool{!leaked, os.Getenv("SITE_OPTION") == "declared", os.Getenv("DEMO_TOKEN") == "injected", !missing, os.Getenv("HTTPS_PROXY") == "proxy"})
			if err != nil {
				return err
			}
		case "alarm/run":
			var a protocol.AlarmRunParams
			if err := json.Unmarshal(*req.Params, &a); err != nil {
				return err
			}
			if a.Reason != "requested" || a.ScheduledAtMs != 42 {
				return errors.New("闹钟参数")
			}
			switch a.AlarmID {
			case "crash":
				os.Exit(9)
			case "block":
				alarmID = req.ID
				if err := output.Encode(protocol.Notification{JSONRPC: "2.0", Method: "entered"}); err != nil {
					return err
				}
				continue
			case "callback":
				if err := fakeCallback(input, output); err != nil {
					return err
				}
			}
		case "$/cancel":
			var p struct{ ID json.RawMessage }
			if err := json.Unmarshal(*req.Params, &p); err != nil {
				return err
			}
			if string(p.ID) != string(alarmID) {
				return errors.New("取消关联")
			}
			if err := output.Encode(protocol.Notification{JSONRPC: "2.0", Method: "canceled"}); err != nil {
				return err
			}
			req.ID = alarmID
		case "action/execute", "action/verify", "context/read", "executor/start":
			result = *req.Params
		case "shutdown":
			if mode == "ignore" || mode == "tree" {
				continue
			}
			if mode != "ack" {
				return os.WriteFile("shutdown", []byte("ok"), 0600)
			}
		}
		if err := output.Encode(protocol.Response{JSONRPC: "2.0", ID: req.ID, Result: result}); err != nil {
			return err
		}
	}
	return input.Err()
}

func fakeCallback(input *bufio.Scanner, output *json.Encoder) error {
	nested := protocol.Request{JSONRPC: "2.0", ID: json.RawMessage(`"nested"`), Method: "work/open"}
	if err := output.Encode(nested); err != nil {
		return err
	}
	if !input.Scan() {
		return errors.New("缺少内核响应")
	}
	var reply protocol.Response
	if err := json.Unmarshal(input.Bytes(), &reply); err != nil {
		return err
	}
	if string(reply.Result) != `"ok"` {
		return errors.New("对向响应")
	}
	return nil
}

func fakeInitialize(mode string, params json.RawMessage) (json.RawMessage, error) {
	var init protocol.InitializeParams
	if err := json.Unmarshal(params, &init); err != nil {
		return nil, err
	}
	if init.Protocol != 1 || init.KernelVersion != "v-test" || string(init.Config) != `{"debug":true}` || init.StateDir == "" {
		return nil, errors.New("握手参数")
	}
	if mode == "stderr" {
		if _, err := io.WriteString(os.Stderr, strings.Repeat("x", 3<<20)+"END\n"); err != nil {
			return nil, err
		}
	}
	if mode == "tree" {
		cmd := exec.Command(os.Args[0], "child")
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		if err := os.WriteFile("pid", []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
			return nil, err
		}
		if err := cmd.Process.Release(); err != nil {
			return nil, err
		}
	}
	if mode == "refuse" {
		return json.RawMessage(`{"ok":false,"capabilities":{}}`), nil
	}
	return json.RawMessage(`{"ok":true,"capabilities":{}}`), nil
}

func fixture(t *testing.T, mode string) (Extension, Options) {
	t.Helper()
	root := t.TempDir()
	t.Setenv(fixtureEnv, mode)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	name := "extension" + filepath.Ext(os.Args[0])
	path := putManifest(t, root, "demo", strings.Replace(manifestText("demo", `,"env":["CRAFT_TEST_EXTENSION","GORACE"]`), "./ext", name, 1))
	dest := filepath.Join(filepath.Dir(path), name)
	// Windows 上临时目录可能位于另一卷，测试不依赖硬链接权限。
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := src.Close(); err != nil {
			t.Error(err)
		}
	}()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY, 0700)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(out, src)
	if cerr := out.Close(); err != nil || cerr != nil {
		t.Fatal(err, cerr)
	}
	res := Discover(root)
	if len(res.Extensions) != 1 {
		t.Fatal(res.Rejected)
	}
	return res.Extensions[0], Options{KernelVersion: "v-test", Config: json.RawMessage(`{"debug":true}`), StateDir: root, LogDir: t.TempDir(), HandshakeTimeout: 5 * time.Second, ShutdownTimeout: time.Second}
}

func launch(t *testing.T, e Extension, o Options) *Host {
	t.Helper()
	h, err := Start(t.Context(), e, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Stop(); err != nil {
			t.Error(err)
		}
	})
	return h
}

func TestHandshakeFailure(t *testing.T) {
	for _, mode := range []string{"version", "timeout", "refuse", "invalid", "long"} {
		t.Run(mode, func(t *testing.T) {
			e, o := fixture(t, mode)
			if mode == "version" {
				e.Manifest.Protocol = 2
			}
			if mode == "timeout" {
				o.HandshakeTimeout = 100 * time.Millisecond
			}
			health := make(chan string, 8)
			o.OnHealth = func(id, state, detail string) {
				if id == e.Manifest.ID && state == "error" {
					health <- detail
				}
			}
			h, err := Start(t.Context(), e, o)
			if h != nil || err == nil {
				t.Fatal(h, err)
			}
			if mode == "version" && !strings.Contains(err.Error(), "protocol") {
				t.Fatal(err)
			}
			select {
			case detail := <-health:
				if detail == "" {
					t.Fatal("健康记录无原因")
				}
			default:
				t.Fatal("缺健康记录")
			}
		})
	}
}

func TestHostProtocol(t *testing.T) {
	e, o := fixture(t, "normal")
	events := make(chan string, 8)
	o.Handler = func(_ context.Context, m string, _ json.RawMessage) (any, error) { events <- m; return "ok", nil }
	h := launch(t, e, o)
	if err := h.AlarmRun(t.Context(), "callback", "requested", 42); err != nil {
		t.Fatal(err)
	}
	if <-events != "work/open" {
		t.Fatal("对向请求")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.AlarmRun(ctx, "block", "requested", 42) }()
	if <-events != "entered" {
		t.Fatal("调用未进入")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if <-events != "canceled" {
		t.Fatal("扩展未收到取消")
	}
	a, err := h.ActionExecute(t.Context(), protocol.ActionExecuteParams{Action: "write", Preview: true, Params: json.RawMessage(`{"n":9007199254740993}`), IdempotencyKey: "k"})
	if err != nil || !strings.Contains(string(a), `"preview":true`) {
		t.Fatal(string(a), err)
	}
	v, err := h.ActionVerify(t.Context(), protocol.ActionVerifyParams{Action: "write", IdempotencyKey: "k"})
	if err != nil || !strings.Contains(string(v), `"idempotencyKey":"k"`) {
		t.Fatal(string(v), err)
	}
	c, err := h.ContextRead(t.Context(), protocol.ContextReadParams{Thread: protocol.ThreadRef{ID: "abc"}})
	if err != nil || !strings.Contains(string(c), `"id":"abc"`) {
		t.Fatal(string(c), err)
	}
	var port struct{ N int }
	if err := h.Call(t.Context(), "executor/start", json.RawMessage(`{"n":3}`), &port); err != nil || port.N != 3 {
		t.Fatal("端口转发", port, err)
	}

	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.Dir, "shutdown")); err != nil {
		t.Fatal(err)
	}
	if err := h.AlarmRun(ctx, "a", "requested", 42); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestStderr(t *testing.T) {
	e, o := fixture(t, "stderr")
	h := launch(t, e, o)
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(o.LogDir, "demo.log"))
	if err != nil || len(data) > logLimit || len(data) < 4 || !strings.HasSuffix(string(data), "END\n") {
		t.Fatalf("日志 %d 字节，%v", len(data), err)
	}
}

func TestForcedStop(t *testing.T) {
	for _, mode := range []string{"ignore", "ack"} {
		t.Run(mode, func(t *testing.T) {
			e, o := fixture(t, mode)
			o.ShutdownTimeout = 100 * time.Millisecond
			h := launch(t, e, o)
			start := time.Now()
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() {
					if err := h.Stop(); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			if d := time.Since(start); d < o.ShutdownTimeout || d > 5*time.Second {
				t.Fatal(d)
			}
		})
	}
}

type timerRequest struct {
	delay time.Duration
	wake  chan time.Time
}
type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	timers chan timerRequest
}

func (c *manualClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *manualClock) After(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	c.timers <- timerRequest{d, ch}
	return ch
}
func (c *manualClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

func TestRestart(t *testing.T) {
	e, o := fixture(t, "normal")
	clock := &manualClock{now: time.Unix(1000, 0), timers: make(chan timerRequest, 8)}
	o.Clock = clock
	o.StablePeriod = time.Hour
	states := make(chan string, 20)
	o.OnHealth = func(_, state, _ string) { states <- state }
	h := launch(t, e, o)
	if <-states != "ok" {
		t.Fatal("首次启动")
	}
	for i, d := range []time.Duration{2 * time.Second, 10 * time.Second, time.Minute, 5 * time.Minute, 5 * time.Minute, 2 * time.Second} {
		if i == 5 {
			clock.advance(o.StablePeriod)
		}
		if err := h.AlarmRun(t.Context(), "ok", "requested", 42); err != nil {
			t.Fatal(err)
		}
		if err := h.AlarmRun(t.Context(), "crash", "requested", 42); !errors.Is(err, ErrUnavailable) {
			t.Fatal(err)
		}
		if <-states != "error" {
			t.Fatal("缺崩溃告警")
		}
		if err := h.AlarmRun(t.Context(), "ok", "requested", 42); !errors.Is(err, ErrUnavailable) {
			t.Fatal("退避期间", err)
		}
		timer := <-clock.timers
		if timer.delay != d {
			t.Fatalf("第 %d 次退避 %v，期望 %v", i, timer.delay, d)
		}
		if i == 5 {
			if err := h.Stop(); err != nil {
				t.Fatal(err)
			}
			timer.wake <- clock.advance(d)
			break
		}
		timer.wake <- clock.advance(d)
		if <-states != "ok" {
			t.Fatal("重启未握手")
		}
	}
	if err := h.AlarmRun(t.Context(), "ok", "requested", 42); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	select {
	case s := <-states:
		t.Fatalf("停止后状态 %s", s)
	default:
	}
}

func TestParentExitWithInheritedPipes(t *testing.T) {
	e, o := fixture(t, "tree")
	clock := &manualClock{now: time.Unix(10, 0), timers: make(chan timerRequest, 1)}
	o.Clock = clock
	entered := make(chan struct{}, 1)
	o.Handler = func(_ context.Context, method string, _ json.RawMessage) (any, error) {
		if method == "entered" {
			entered <- struct{}{}
		}
		return nil, nil
	}
	h := launch(t, e, o)
	// 主进程已退出，但孙进程仍握着 stdout 和 stderr，不能依赖 EOF 感知崩溃。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pending := make(chan error, 1)
	go func() { pending <- h.AlarmRun(ctx, "block", "requested", 42) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := h.AlarmRun(ctx, "crash", "requested", 42); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if err := <-pending; !errors.Is(err, ErrUnavailable) {
		t.Fatal("崩溃时的在途调用", err)
	}

	select {
	case <-clock.timers:
	case <-ctx.Done():
		t.Fatal("未回收进程树")
	}
}

func TestChildEnvironment(t *testing.T) {
	e, o := fixture(t, "normal")
	t.Setenv("ANTHROPIC_API_KEY", "private")
	t.Setenv("SITE_OPTION", "declared")
	t.Setenv("DEMO_TOKEN", "ambient")
	t.Setenv("ABSENT_TOKEN", "ambient")
	t.Setenv("HTTPS_PROXY", "proxy")
	e.Manifest.Env = append(e.Manifest.Env, "SITE_OPTION", "DEMO_TOKEN", "ABSENT_TOKEN")
	e.Manifest.Credentials = []CredentialDecl{{Name: "default", Env: "DEMO_TOKEN"}, {Name: "other", Env: "ABSENT_TOKEN"}}
	o.GetCredential = func(provider, name string) (string, bool, error) {
		if provider != "demo" {
			t.Fatalf("错误的 provider %s", provider)
		}
		return "injected", name == "default", nil
	}
	var state, detail string
	o.OnHealth = func(_ string, s, d string) { state, detail = s, d }
	var observed []bool
	h := launch(t, e, o)
	if err := h.Call(t.Context(), "fixture/environment", struct{}{}, &observed); err != nil {
		t.Fatal(err)
	}
	if state != "warning" || !strings.Contains(detail, "demo/other") || strings.Contains(detail, "injected") || len(observed) != 5 {
		t.Fatal("缺少安全的健康告警或子进程结果")
	}
	for i, ok := range observed {
		if !ok {
			t.Errorf("子进程环境检查 %d 失败", i)
		}
	}
}
