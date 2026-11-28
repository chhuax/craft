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
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

func runActionFixture() {
	ready := make(chan *exthost.Conn, 1)
	var conn *exthost.Conn
	var once sync.Once
	input, forward := io.Pipe()
	c := exthost.NewConn(input, os.Stdout, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		once.Do(func() { conn = <-ready })
		switch method {
		case "initialize":
			return protocol.InitializeResult{OK: true}, nil
		case "shutdown":
			os.Exit(0)
		case "action/verify":
			var p protocol.ActionVerifyParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			if p.Action != "interrupt" {
				return nil, fmt.Errorf("意外核实 %s", p.Action)
			}
			sent, err := os.ReadFile("sent")
			if err != nil {
				return nil, err
			}
			if !slices.Contains(strings.Fields(string(sent)), p.IdempotencyKey) {
				return map[string]string{"status": "not_applied"}, nil
			}
			return map[string]any{"status": "applied", "receipt": map[string]string{"verified": p.IdempotencyKey}}, nil
		case "action/execute":
			var p protocol.ActionExecuteParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			if strings.HasSuffix(p.Action, ":receipts") {
				if p.Preview {
					return nil, errors.New("只读动作不应预览")
				}
				var v json.RawMessage
				err := conn.Call(ctx, "receipts/recent", p.Params, &v)
				return v, err
			}
			if p.Preview {
				return map[string]string{"summary": "预览", "target": "one"}, nil
			}
			if p.Action == "bench" {
				return map[string]string{"sent": p.IdempotencyKey}, nil
			}
			f, err := os.OpenFile("sent", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
			if err != nil {
				return nil, err
			}
			_, err = fmt.Fprintln(f, p.IdempotencyKey)
			if err = errors.Join(err, f.Close()); err != nil {
				return nil, err
			}
			if p.Action == "interrupt" {
				os.Exit(9)
			}
			return map[string]string{"sent": p.IdempotencyKey}, nil
		}
		return nil, exthost.ErrMethodNotFound
	}, 0)
	ready <- c
	_, err := io.Copy(forward, os.Stdin)
	_ = forward.CloseWithError(err)
}

func actionManifest(t testing.TB, home, id, actions string) {
	t.Helper()
	dir := filepath.Join(home, "extensions", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	name := "fixture" + filepath.Ext(exe)
	if err := os.Link(exe, filepath.Join(dir, name)); err != nil {
		data, err := os.ReadFile(exe)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	raw := fmt.Sprintf(`{"id":%q,"version":"1","protocol":1,"exec":[%q,"--action-fixture"],"actions":[%s]}`, id, name, actions)
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

type actionClock struct {
	alarm.RealClock
	minute atomic.Int64
}

func (c *actionClock) Now() time.Time { return time.Unix(1000+60*c.minute.Load(), 0).UTC() }

func serveActions(t testing.TB, home string, clock *actionClock) (daemonState, func()) {
	t.Helper()
	d := app.NewDaemon(app.DaemonConfig{StateDir: home, ExtensionDirs: []string{filepath.Join(home, "extensions")}, Version: "dev", Clock: clock})
	if err := d.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st := daemonState{Port: ln.Addr().(*net.TCPAddr).Port, PID: os.Getpid(), StartedAt: clock.Now(), Version: "dev", Token: strings.Repeat("x", 32)}
	api := localAPI{state: st, home: home, daemon: d}
	server := &http.Server{Handler: api.handler(), ReadHeaderTimeout: time.Second}
	go func() {
		if err := server.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	}()
	if err := st.publish(home); err != nil {
		t.Fatal(err)
	}
	stop := sync.OnceFunc(func() {
		if err := errors.Join(server.Shutdown(context.Background()), d.Stop(context.Background())); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(stop)
	return st, stop
}

func actionCommand(t *testing.T, b *app.SessionBinding, want int, args ...string) map[string]json.RawMessage {
	t.Helper()
	var out, diag bytes.Buffer
	code := RunWithContext(append(args, "--json"), "dev", &out, &diag, CommandContext{Session: b})
	if code != want {
		t.Fatalf("%v: %d，期望 %d，%s%s", args, code, want, &out, &diag)
	}
	var r struct {
		Payload map[string]json.RawMessage
		Error   struct {
			Code   string
			Detail map[string]json.RawMessage
		}
	}
	raw := out.Bytes()
	if want != 0 {
		raw = diag.Bytes()
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err, string(raw))
	}
	if want != 0 {
		if want == 3 && (r.Error.Code != "needs_confirmation" || !bytes.Contains(r.Error.Detail["preview"], []byte("预览"))) {
			t.Fatal(string(raw))
		}
		return r.Error.Detail
	}
	return r.Payload
}

func TestLocalActions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	actionManifest(t, home, "first", `{"id":"send","level":"auto","reversible":true},{"id":"ask","level":"confirm"},{"id":"strict","level":"auto"},{"id":"blocked","level":"deny"},{"id":"interrupt","level":"auto"},{"id":"first:receipts","level":"auto","read_only":true}`)
	actionManifest(t, home, "second", `{"id":"second:receipts","level":"auto","read_only":true}`)
	if err := os.WriteFile(filepath.Join(home, "outbound.json"), []byte(`{"organization":{"strict":"confirm_named","blocked":"auto"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	clock := &actionClock{}
	st, stop := serveActions(t, home, clock)
	act := func(want int, action, params string, rest ...string) map[string]json.RawMessage {
		t.Helper()
		return actionCommand(t, nil, want, append([]string{"act", action, "--params", params}, rest...)...)
	}
	sends := func() []string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(home, "extensions", "first", "sent"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return strings.Fields(string(raw))
	}
	act(3, "ask", "{}", "--key", "wait")
	worker := &app.SessionBinding{Session: engine.Session{Host: "host", ID: "session"}, Assignment: "assignment"}
	actionCommand(t, worker, 3, "act", "ask", "--params", `{"source":"session"}`, "--yes", "--key", "worker")
	if r := actionCommand(t, nil, 0, "outbox", "show", "worker"); string(r["source"]) != `"worker"` {
		t.Fatal(r)
	}
	actionCommand(t, worker, 4, "outbox", "confirm", "worker", "--yes")
	act(4, "blocked", "{}", "--yes")
	act(4, "strict", "{}", "--yes")
	act(3, "send", `{"text":"Bearer abcdefghijkl"}`, "--key", "content")
	if n := len(sends()); n != 0 {
		t.Fatal("预览或拒绝产生了外部效果", n)
	}
	r := act(0, "send", `{"z":9007199254740993,"a":0}`)
	if string(r["state"]) != `"done"` || string(r["reversible"]) != "true" {
		t.Fatal(r)
	}
	act(0, "send", `{ "a":0, "z":9007199254740993 }`)
	if n := len(sends()); n != 1 {
		t.Fatal("幂等重放", n)
	}
	if r := act(0, "ask", "{}", "--yes", "--key", "other"); string(r["state"]) != `"done"` {
		t.Fatal(r)
	}
	act(0, "ask", "{}", "--yes", "--key", "other")
	if r := act(0, "second:receipts", `{"limit":10,"extension":"first"}`); !bytes.Contains(r["receipt"], []byte(`"items":[]`)) {
		t.Fatal(r)
	}
	page := func(after int64, wantID string) int64 {
		t.Helper()
		raw := act(0, "first:receipts", fmt.Sprintf(`{"afterSentSeq":%d,"limit":1}`, after))["receipt"]
		var v struct {
			Items []struct {
				SentSeq int64
				ID      string
				Receipt json.RawMessage
			}
		}
		if err := json.Unmarshal(raw, &v); err != nil || len(v.Items) != 1 || v.Items[0].ID != wantID || v.Items[0].SentSeq <= after || len(v.Items[0].Receipt) == 0 {
			t.Fatal(string(raw), err)
		}
		return v.Items[0].SentSeq
	}
	var firstID string
	if err := json.Unmarshal(r["id"], &firstID); err != nil {
		t.Fatal(err)
	}
	cursor := page(page(0, firstID), "other")
	snapshot := func(params any) map[string]json.RawMessage {
		t.Helper()
		raw, err := st.call(t.Context(), "snapshot", params)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(raw, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := snapshot(nil)
	if !bytes.Contains(before["attention"], []byte(`"kind":"confirm"`)) {
		t.Fatal(before)
	}
	if r := actionCommand(t, nil, 0, "watch", "status"); string(r["attention"]) != "4" {
		t.Fatal(r)
	}
	if r := actionCommand(t, nil, 0, "watch", "attention"); !bytes.Contains(r["items"], []byte(`"kind":"confirm"`)) {
		t.Fatal(r)
	}
	actionCommand(t, nil, 3, "outbox", "confirm", "wait")
	actionCommand(t, nil, 0, "outbox", "confirm", "wait", "--yes")
	page(cursor, "wait")
	actionCommand(t, nil, 4, "outbox", "withdraw", "wait")
	actionCommand(t, nil, 0, "outbox", "withdraw", "worker")
	actionCommand(t, nil, 4, "outbox", "confirm", "worker", "--yes")
	after := snapshot(map[string]any{"sinceRev": before["rev"]})
	if !bytes.Contains(after["counts"], []byte(`"attention":2`)) {
		t.Fatal(after)
	}
	act(6, "missing", "{}", "--key", "missing")
	if r := actionCommand(t, nil, 0, "outbox", "show", "missing"); string(r["level"]) != `"confirm"` {
		t.Fatal(r)
	}
	act(6, "interrupt", "{}", "--key", "crash")
	if r := actionCommand(t, nil, 0, "outbox", "show", "crash"); string(r["state"]) != `"executing"` {
		t.Fatal(r)
	}
	if r := actionCommand(t, nil, 0, "daemon", "status"); !bytes.Contains(r["health"], []byte(`"executing":1`)) {
		t.Fatal(r)
	}
	stop()
	_, _ = serveActions(t, home, clock)
	if r := act(0, "interrupt", "{}", "--key", "crash"); !bytes.Contains(r["receipt"], []byte(`"verified":"crash"`)) {
		t.Fatal(r)
	}
	if n := len(sends()); n != 4 {
		t.Fatal("恢复重复执行", sends())
	}
	if r := actionCommand(t, nil, 0, "outbox", "list"); len(r["items"]) == 0 {
		t.Fatal(r)
	}
}

func TestNamedTargetAndRuleReload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	actionManifest(t, home, "first", `{"id":"named","level":"confirm_named"}`)
	st, stop := serveActions(t, home, &actionClock{})
	actionCommand(t, nil, 3, "act", "named", "--params", "{}", "--key", "n")
	for _, method := range []string{"act", "outbox.confirm"} {
		p := map[string]any{"action": "named", "params": map[string]any{}, "idempotencyKey": "n", "id": "n", "yes": true, "namedTarget": "other"}
		_, err := st.call(t.Context(), method, p)
		if _, exit := rpcFailure(err); exit != 4 {
			t.Fatalf("%s: %v", method, err)
		}
	}
	p := map[string]any{"id": "n", "yes": true, "namedTarget": "one"}
	if _, err := st.call(t.Context(), "outbox.confirm", p); err != nil {
		t.Fatal(err)
	}
	actionCommand(t, nil, 3, "act", "named", "--params", "{}", "--key", "recheck")
	stop()
	if err := os.WriteFile(filepath.Join(home, "outbound.json"), []byte(`{"organization":{"named":"deny"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	serveActions(t, home, &actionClock{})
	actionCommand(t, nil, 4, "outbox", "confirm", "recheck")
	actionCommand(t, nil, 4, "outbox", "confirm", "recheck", "--yes")
}

func TestOutboxOwnerChanges(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	actionManifest(t, home, "first", `{"id":"interrupt","level":"auto"},{"id":"ask","level":"confirm"}`)
	_, stop := serveActions(t, home, &actionClock{})
	actionCommand(t, nil, 0, "act", "ask", "--params", "{}", "--yes", "--key", "done")
	actionCommand(t, nil, 3, "act", "ask", "--params", "{}", "--key", "pending")
	actionCommand(t, nil, 6, "act", "interrupt", "--params", "{}", "--key", "crash")
	stop()
	// 在核实前换动作所有者。新扩展没有证据为旧发送结案，也不能读旧扩展的回执。
	if err := os.Rename(filepath.Join(home, "extensions", "first"), filepath.Join(home, "old")); err != nil {
		t.Fatal(err)
	}
	actionManifest(t, home, "second", `{"id":"interrupt","level":"auto"},{"id":"ask","level":"confirm"},{"id":"second:receipts","level":"auto","read_only":true}`)
	serveActions(t, home, &actionClock{})
	r := actionCommand(t, nil, 0, "daemon", "status")
	var health app.DaemonHealth
	if err := json.Unmarshal(r["health"], &health); err != nil || health.Outbox != (app.OutboxCounts{Pending: 1, Unknown: 1}) {
		t.Fatal(health, err)
	}
	actionCommand(t, nil, 4, "outbox", "confirm", "pending", "--yes")
	r = actionCommand(t, nil, 0, "act", "second:receipts", "--params", `{"limit":10}`)
	if !bytes.Contains(r["receipt"], []byte(`"items":[]`)) {
		t.Fatal(r)
	}
}

func BenchmarkLocalActAuto(b *testing.B) {
	home := b.TempDir()
	actionManifest(b, home, "first", `{"id":"bench","level":"auto"}`)
	clock := &actionClock{}
	st, _ := serveActions(b, home, clock)
	b.ReportAllocs()
	for b.Loop() {
		n := clock.minute.Add(1)
		if _, err := st.call(b.Context(), "act", map[string]any{"action": "bench", "params": map[string]string{}, "idempotencyKey": strconv.FormatInt(n, 10)}); err != nil {
			b.Fatal(err)
		}
	}
}
