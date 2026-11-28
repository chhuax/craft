package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "daemon":
			os.Exit(Run(os.Args[1:], "dev", os.Stdout, os.Stderr))
		case "hook":
			os.Exit(runBound(os.Args[1:], "dev", os.Stdout, os.Stderr, os.Getenv, os.UserHomeDir,
				CommandContext{Session: app.SessionFromEnv(os.Getenv)}, func() string { return testNow }))
		case "--action-fixture":
			runActionFixture()
			os.Exit(0)
		case "--alarm-fixture":
			ready := make(chan *exthost.Conn, 1)
			var first sync.Once
			input, forward := io.Pipe()
			conn := exthost.NewConn(input, os.Stdout, func(ctx context.Context, method string, _ json.RawMessage) (any, error) {
				switch method {
				case "initialize":
					return protocol.InitializeResult{OK: true, Capabilities: json.RawMessage(`{}`)}, nil
				case "alarm/run":
					var gateErr error
					first.Do(func() {
						conn, err := net.Dial("tcp4", os.Args[2])
						if err != nil {
							gateErr = err
							return
						}
						var signal [1]byte
						_, err = io.ReadFull(conn, signal[:])
						gateErr = errors.Join(err, conn.Close())
					})
					if gateErr != nil {
						return nil, gateErr
					}
					c := <-ready
					defer func() { ready <- c }()
					return nil, c.Call(ctx, "thread/describe", map[string]string{"key": "feed:item:sample", "label": "alarm"}, nil)
				case "shutdown":
					os.Exit(0)
				}
				return nil, exthost.ErrMethodNotFound
			}, 0)
			ready <- conn
			_, err := io.Copy(forward, os.Stdin)
			_ = forward.CloseWithError(err)
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func daemonCommand(t testing.TB, args ...string) map[string]any {
	t.Helper()
	var out, diag bytes.Buffer
	code := Run(append(args, "--json"), "dev", &out, &diag)
	if code != 0 {
		t.Fatalf("%v: %d %s %s", args, code, &out, &diag)
	}
	var v struct{ Payload map[string]any }
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v.Payload
}

func TestProcessLifecycle(t *testing.T) {
	home := t.TempDir()
	// 子进程会消费人工请求的 launch；本用例只用临时目录中的扩展。
	t.Setenv("PATH", home)
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	ext := filepath.Join(home, "extensions", "sample")
	if err := os.MkdirAll(ext, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "fixture" + filepath.Ext(exe)
	if err := os.Link(exe, filepath.Join(ext, name)); err != nil {
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(ext, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close() }()
	manifest := fmt.Sprintf(`{"id":"sample","version":"1","protocol":1,"exec":[%q,"--alarm-fixture",%q],"watch_sources":[{"id":"feed","thread_kinds":[{"id":"item"}]}],"alarms":[{"id":"poll","every":"1h"}]}`, name, gate.Addr().String())
	if err := os.WriteFile(filepath.Join(ext, "manifest.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	daemonCommand(t, "daemon", "start")
	t.Cleanup(func() { Run([]string{"daemon", "stop"}, "dev", io.Discard, io.Discard) })
	status := daemonCommand(t, "daemon", "status")
	if status["running"] != true || status["version"] != "dev" {
		t.Fatal(status)
	}
	if d := daemonCommand(t, "daemon", "start"); d["alreadyRunning"] != true {
		t.Fatal(d)
	}
	state, err := readDaemon(home)
	if err != nil {
		t.Fatal(err)
	}
	outsider, err := http.NewRequestWithContext(t.Context(), "POST", "http://"+state.address()+"/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"status"}`))
	if err != nil {
		t.Fatal(err)
	}
	outsider.Header.Set("Content-Type", "application/json")
	denied, err := rpcClient.Do(outsider)
	if err != nil {
		t.Fatal(err)
	}
	if err := denied.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if denied.StatusCode != http.StatusUnauthorized {
		t.Fatal("无令牌进程读取了状态", denied.Status)
	}

	anonymous := state
	anonymous.Token = strings.Repeat("x", len(state.Token))
	if _, err := anonymous.call(t.Context(), "status", nil); !errors.Is(err, errDependency) {
		t.Fatal("未拒绝另一个本地进程的无效令牌", err)
	}
	func() {
		if err := anonymous.publish(home); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := state.publish(home); err != nil {
				t.Error(err)
			}
		}()
		command(t, home, 6, "daemon", "status")
		command(t, home, 6, "ext", "list")
	}()
	var diag, out bytes.Buffer
	if code := Run([]string{"daemon", "status", "--json"}, "new-version", &out, &diag); code != 0 || !strings.Contains(out.String(), `"fingerprintStale":true`) || diag.Len() == 0 {
		t.Fatal(code, out.String(), diag.String())
	}
	if after, err := readDaemon(home); err != nil || after.Token != state.Token {
		t.Fatal("提示过期后实例发生变化", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	raw, err := state.call(ctx, "snapshot", nil)
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Rev    int64
		Counts json.RawMessage
	}
	if err := json.Unmarshal(raw, &snap); err != nil || len(snap.Counts) == 0 {
		t.Fatal(string(raw), err)
	}
	raw, err = state.call(ctx, "snapshot", map[string]int64{"sinceRev": snap.Rev})
	if err != nil || !bytes.Contains(raw, []byte(`"unchanged":true`)) {
		t.Fatal(string(raw), err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://%s/events?since=%d", state.address(), snap.Rev), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+state.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	if q := daemonCommand(t, "alarm", "run", "sample", "poll", "--wait", "0"); q["timedOut"] != true || q["queued"] != true {
		t.Fatal(q)
	}
	if err := gate.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ack, err := gate.Accept()
	if err != nil {
		t.Fatal(err)
	}
	_, err = ack.Write([]byte{1})
	if err = errors.Join(err, ack.Close()); err != nil {
		t.Fatal(err)
	}
	var chunk [2048]byte
	var stream string
	for !strings.Contains(stream, "thread.described") {
		n, err := resp.Body.Read(chunk[:])
		stream += string(chunk[:n])
		if err != nil {
			t.Fatal(err, stream)
		}
	}
	if !strings.Contains(stream, fmt.Sprintf("id: %d\n", snap.Rev+1)) {
		t.Fatal(stream)
	}
	// 流保持打开，从另一个进程的写连接提交，不依赖本进程的通知。
	daemonCommand(t, "watch", "request", "feed:item:external")
	for !strings.Contains(stream, "external") {
		n, err := resp.Body.Read(chunk[:])
		stream += string(chunk[:n])
		if err != nil {
			t.Fatal(err, stream)
		}
	}
	raw, err = state.call(ctx, "events", map[string]any{"sinceRev": snap.Rev, "limit": 2})
	if err != nil {
		t.Fatal(err)
	}
	var replay eventBatch
	if err := json.Unmarshal(raw, &replay); err != nil || len(replay.Items) != 2 || replay.Items[0].Rev != snap.Rev+1 || replay.Items[1].Rev != snap.Rev+2 || replay.NextSince != snap.Rev+2 {
		t.Fatal(string(raw), err)
	}
	if q := daemonCommand(t, "alarm", "run", "sample", "poll", "--wait"); q["status"] != "done" {
		t.Fatal(q)
	}
	daemonCommand(t, "daemon", "stop")
	if c := Run([]string{"alarm", "list"}, "dev", io.Discard, io.Discard); c != 6 {
		t.Fatal(c)
	}
}

func TestTakeover(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	daemonCommand(t, "daemon", "start")
	t.Cleanup(func() { Run([]string{"daemon", "stop"}, "dev", io.Discard, io.Discard) })
	old, err := readDaemon(home)
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.FindProcess(old.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Release() }()
	proc, err := observeExit(old.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := proc.close(); err != nil {
			t.Error(err)
		}
	}()
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	// 状态文件仍然存在；等待操作系统真正释放持有者的锁。
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if err := proc.wait(ctx); err != nil {
		t.Fatal(err)
	}
	daemonCommand(t, "daemon", "start")
	now, err := readDaemon(home)
	if err != nil || now.Token == old.Token {
		t.Fatal(err)
	}
}

func TestOnlyOneOwner(t *testing.T) {
	home := t.TempDir()
	// 模拟旧 PID 正好被本进程复用，且启动者尚未发布新的地址。
	if err := os.WriteFile(filepath.Join(home, "daemon.lock"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wg sync.WaitGroup
	wins := make(chan *daemonLease, n)
	for range n {
		wg.Go(func() {
			l, err := claimDaemon(t.Context(), home)
			if err == nil {
				wins <- l
			}
		})
	}
	wg.Wait()
	close(wins)
	if len(wins) != 1 {
		t.Fatalf("持有者数 %d", len(wins))
	}
	if err := (<-wins).close(); err != nil {
		t.Fatal(err)
	}
}

func TestInterfaceGuards(t *testing.T) {
	st := daemonState{Port: 12345, Token: strings.Repeat("z", 32)}
	api := localAPI{state: st}
	for _, tc := range []struct {
		method, path, host, auth, typ string
		origin, oversize              bool
		want                          int
	}{
		{"POST", "/rpc", "127.0.0.1:12345", "", "application/json", false, false, 401},
		{"GET", "/events", "127.0.0.1:12345", "wrong", "", false, false, 401},
		{"POST", "/rpc", "untrusted.example:12345", st.Token, "application/json", false, false, 403},
		{"POST", "/rpc", "localhost:99", st.Token, "application/json", false, false, 403},
		{"POST", "/rpc", "localhost:12345", st.Token, "application/json", true, false, 403},
		{"GET", "/events", "localhost:12345", st.Token, "", true, false, 403},
		{"GET", "/rpc", "localhost:12345", st.Token, "application/json", false, false, 405},
		{"POST", "/rpc", "localhost:12345", st.Token, "text/plain", false, false, 415},
		{"POST", "/rpc", "localhost:12345", st.Token, "application/json", false, true, 413},
		{"POST", "/rpc", "localhost:12345", st.Token, "application/json", false, false, 200},
	} {
		body := `{"jsonrpc":"2.0","method":"unknown","id":1}`
		if tc.oversize {
			body += strings.Repeat(" ", maxRPCBody)
		}
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(body))
		r.Host = tc.host
		if tc.auth != "" {
			r.Header.Set("Authorization", "Bearer "+tc.auth)
		}
		if tc.origin {
			origin := "https://untrusted.example"
			if tc.path == "/events" {
				origin = ""
			}
			r.Header.Set("Origin", origin)
		}
		r.Header.Set("Content-Type", tc.typ)
		w := httptest.NewRecorder()
		api.handler().ServeHTTP(w, r)
		if w.Code != tc.want || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%+v: %d", tc, w.Code)
		}
		if tc.want == 200 && !strings.Contains(w.Body.String(), "-32601") {
			t.Fatal(w.Body.String())
		}
	}
}

func TestConcurrentProcesses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	// 所有争用者都碰到同一份陈旧锁，先死的进程恰好用过测试进程的 PID。
	if err := os.WriteFile(filepath.Join(home, "daemon.lock"), []byte(fmt.Sprint(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const n = 6
	done := make(chan error, n)
	start := make(chan struct{})
	for range n {
		go func() { <-start; done <- exec.CommandContext(ctx, exe, "daemon", "run", "--json").Run() }()
	}
	close(start)
	for range n - 1 {
		err := <-done
		if exit, ok := errors.AsType[*exec.ExitError](err); !ok || exit.ExitCode() != 6 {
			t.Fatal("争用退出结果", err)
		}
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if s, err := readDaemon(home); err == nil {
			if _, err = s.status(ctx); err == nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	daemonCommand(t, "daemon", "stop")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotPages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	seedWatch(t, home, 100)
	daemonCommand(t, "daemon", "start")
	t.Cleanup(func() { daemonCommand(t, "daemon", "stop") })
	st, err := readDaemon(home)
	if err != nil {
		t.Fatal(err)
	}
	var cursor string
	seen := map[string]bool{}
	for {
		raw, err := st.call(t.Context(), "snapshot", map[string]string{"workCursor": cursor})
		if err != nil {
			t.Fatal(err)
		}
		var snap struct {
			Rev       int64
			Counts    struct{ Threads, Works, Attention int }
			OpenWorks struct {
				Items      []workRow
				NextCursor string
			}
			Attention struct{ Items []json.RawMessage }
		}
		if err := json.Unmarshal(raw, &snap); err != nil {
			t.Fatal(err)
		}
		if snap.Counts.Threads != 100 || snap.Counts.Works != 1000 || len(snap.Attention.Items) != snap.Counts.Attention || len(snap.OpenWorks.Items) > pageSize {
			t.Fatal(snap.Counts)
		}
		for _, w := range snap.OpenWorks.Items {
			if seen[w.ID] {
				t.Fatal("重复工作", w.ID)
			}
			seen[w.ID] = true
		}
		cursor = snap.OpenWorks.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 1000 {
		t.Fatal(len(seen))
	}
}
