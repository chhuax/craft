package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "--d1-extension" {
		runTestExtension()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func runTestExtension() {
	ready := make(chan *exthost.Conn, 1)
	var conn *exthost.Conn
	var once sync.Once
	var calls atomic.Int32
	release := make(chan struct{})
	c := exthost.NewConn(os.Stdin, os.Stdout, func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		once.Do(func() { conn = <-ready })
		ref := protocol.ThreadRef{Source: "feed", Kind: "item", ID: "a:b"}
		switch method {
		case "initialize":
			return protocol.InitializeResult{OK: true, Capabilities: json.RawMessage(`{}`)}, nil
		case "alarm/run":
			var p protocol.AlarmRunParams
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			if p.AlarmID == "hold" {
				entry := "again"
				if calls.Add(1) == 1 {
					// 父进程看到 entered 就能拨钟，确认回包仍可能在管道中。等待确认不能被这次取消打断。
					if err := conn.Call(context.WithoutCancel(ctx), "health/report", map[string]string{"entry": "entered", "state": "ok", "detail": p.Reason}, nil); err != nil {
						return nil, err
					}
					<-ctx.Done()
					if err := conn.Call(context.WithoutCancel(ctx), "health/report", map[string]string{"entry": "canceled", "state": "ok"}, nil); err != nil {
						return nil, err
					}
					<-release
					return nil, nil
				}
				return nil, conn.Call(ctx, "health/report", map[string]string{"entry": entry, "state": "ok", "detail": p.Reason}, nil)
			}
			if p.AlarmID != "poll" || p.Reason != "schedule" && p.Reason != "requested" {
				return nil, errors.New("闹钟参数不符")
			}
			return nil, conn.Call(ctx, "watch/receive", protocol.Trigger{ID: "event", Source: "feed", Event: "change", At: time.UnixMilli(p.ScheduledAtMs).UTC().Format(time.RFC3339Nano), Threads: []protocol.ThreadRef{ref}}, nil)
		case "fixture/describe":
			return nil, conn.Call(ctx, "thread/describe", map[string]string{"key": "other:item:a:b", "label": "owned by second"}, nil)
		case "fixture/close", "fixture/foreign":
			if method == "fixture/foreign" {
				ref.Source = "other"
			}
			var p struct {
				At string `json:"at"`
			}
			if err := json.Unmarshal(raw, &p); err != nil {
				return nil, err
			}
			return nil, conn.Call(ctx, "thread/closed", map[string]any{"thread": ref, "at": p.At, "outcome": "done"}, nil)
		case "fixture/release":
			close(release)
			return nil, nil
		case "fixture/crash":
			os.Exit(8)
		case "shutdown":
			// shutdown 的最后一次对向调用必须仍可访问库。
			if err := conn.Call(ctx, "state/set", map[string]any{"key": "shutdown", "value": true}, nil); err != nil {
				return nil, err
			}
			if err := conn.Call(ctx, "health/report", map[string]string{"entry": "shutdown", "state": "ok"}, nil); err != nil {
				return nil, err
			}
			os.Exit(0)
		}
		return nil, exthost.ErrMethodNotFound
	}, 0)
	ready <- c
	select {}
}

type daemonTestTimer struct {
	at time.Time
	ch chan time.Time
}
type daemonTestClock struct {
	mu         sync.Mutex
	now        time.Time
	timers     []daemonTestTimer
	registered chan struct{}
}

func newTestClock() *daemonTestClock {
	return &daemonTestClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), registered: make(chan struct{})}
}

func (c *daemonTestClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *daemonTestClock) NewTimer(d time.Duration) *time.Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := time.NewTimer(time.Hour * 24)
	t.Stop()
	ch := make(chan time.Time, 1)
	t.C = ch
	if d <= 0 {
		ch <- c.now
	} else {
		c.timers = append(c.timers, daemonTestTimer{c.now.Add(d), ch})
	}
	close(c.registered)
	c.registered = make(chan struct{})
	return t
}

func (c *daemonTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	remaining := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(c.now) {
			remaining = append(remaining, tm)
		} else {
			tm.ch <- c.now
		}
	}
	c.timers = remaining
}

// 首先确认指定时刻的所有计时器均已登记，再推进另一进程参与的试验。
func (c *daemonTestClock) waitTimers(t *testing.T, delay time.Duration, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for {
		c.mu.Lock()
		n := 0
		for _, timer := range c.timers {
			if timer.at.Equal(c.now.Add(delay)) {
				n++
			}
		}
		changed := c.registered
		c.mu.Unlock()
		if n >= count {
			return
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatalf("等待 %d 个 %v 定时器，已登记 %d", count, delay, n)
		}
	}
}

func installTestManifest(t *testing.T, root, id, src string, major int, executable bool) string {
	t.Helper()
	dir := filepath.Join(root, id)
	checkErr(t, os.MkdirAll(dir, 0700))
	name := "fixture" + filepath.Ext(os.Args[0])
	if executable {
		in, err := os.Open(os.Args[0])
		checkErr(t, err)
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE, 0700)
		checkErr(t, err)
		_, err = io.Copy(out, in)
		checkErr(t, errors.Join(err, in.Close(), out.Close()))
	}
	manifest := fmt.Sprintf(`{"id":%q,"version":"1","protocol":%d,"exec":[%q,"--d1-extension"],"watch_sources":[{"id":%q,"thread_kinds":[{"id":"item","closable":true}],"events":[{"id":"change","default":"auto","strength":"explicit"}]}],"alarms":[{"id":"poll","every":"1h"}]}`, id, major, name, src)
	p := filepath.Join(dir, "manifest.json")
	checkErr(t, os.WriteFile(p, []byte(manifest), 0600))
	return p
}

func TestDaemonEndToEnd(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	root := t.TempDir()
	manifest := installTestManifest(t, root, "reader", "feed", 1, true)
	installTestManifest(t, root, "second", "other", 1, true)
	bad := installTestManifest(t, root, "forged", "craft", 1, false)
	installTestManifest(t, root, "future", "future", 2, false)
	clock := newTestClock()
	var d *Daemon
	changed := make(chan struct{}, 1)
	var checkShutdown sync.Once
	onHealth := func() {
		if d.Health().Reports["reader"]["shutdown"].Status == "ok" {
			checkShutdown.Do(func() {
				select {
				case <-d.runtime.done:
				default:
					t.Error("shutdown 前 Pass 尚未退出")
				}
				select {
				case <-d.runtime.effects.(*effectScheduler).active.done:
				default:
					t.Error("shutdown 前 effect 尚未退出")
				}
				for _, s := range d.alarms.List() {
					if s.State != "stopped" {
						t.Errorf("shutdown 前闹钟状态 %v", s)
					}
				}
			})
		}
		select {
		case changed <- struct{}{}:
		default:
		}
	}
	stateDir := t.TempDir()
	d = NewDaemon(DaemonConfig{StateDir: stateDir, ExtensionDirs: []string{root}, Clock: clock, Version: "test", OnHealth: onHealth, AlarmOverrides: map[string]AlarmOverride{"second:poll": {Enabled: new(false)}, "reader:poll": {Every: 2 * time.Hour}}})
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	checkErr(t, d.Start(ctx))
	t.Cleanup(func() { checkErr(t, d.Stop(context.Background())) })
	health := d.Health()
	if health.Rejected[bad].Status != "rejected" || health.Extensions["future"].Status != "incompatible" || health.Extensions["reader"].Status != "ready" {
		t.Fatal(health)
	}
	created := make(chan string, 1)
	d.runtime.repo.Subscribe(func(e engine.Event) {
		if e.Type == "decision.recorded" {
			created <- e.Ref["decision"]
		}
	})
	alarms := d.alarms.List()
	if len(alarms) != 3 || alarms[2].ID != "watch.sessions" || alarms[0].ID != "reader:poll" || !alarms[0].Next.Equal(clock.Now().Add(2*time.Hour)) || alarms[1].State != "disabled" {
		t.Fatalf("用户闹钟覆盖 %v", alarms)
	}
	clock.waitTimers(t, 10*time.Second, 1)
	clock.waitTimers(t, passPeriod, 2)
	clock.advance(2 * time.Hour)
	var decision string
	select {
	case decision = <-created:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	var work string
	checkErr(t, d.runtime.repo.Read(ctx, func(r engine.Reader) error {
		dec, err := r.GetDecision(decision)
		if err != nil {
			return err
		}
		if dec == nil || dec.Kind != engine.Accept {
			t.Fatalf("研判 %v", dec)
		}
		work = dec.WorkID
		w, err := r.GetWork(work)
		if err != nil {
			return err
		}
		if len(w.Assignments) != 1 || w.Assignments[0].State != engine.AssignmentPending {
			t.Error("等待目标的派工应保持 pending")
		}
		return nil
	}))
	h := d.hosts["reader"]
	at := map[string]string{"at": clock.Now().Format(time.RFC3339Nano)}
	checkErr(t, d.hosts["second"].Call(ctx, "fixture/describe", struct{}{}, nil))
	err := h.Call(ctx, "fixture/foreign", at, nil)
	rpc, ok := errors.AsType[*protocol.Error](err)
	if !ok || rpc.Code != -32602 {
		t.Fatalf("越权结果 %v", err)
	}
	checkErr(t, d.runtime.repo.Read(ctx, func(r engine.Reader) error {
		th, err := r.GetThread("other:item:a:b")
		if th == nil || th.Closed != nil || th.Meta.Label != "owned by second" {
			t.Fatal("越权写入")
		}
		return err
	}))
	// 两个周期循环已重新登记，第三个同刻定时器才是 Host 的重启退避。
	clock.waitTimers(t, passPeriod, 2)
	if err := h.Call(ctx, "fixture/crash", struct{}{}, nil); !errors.Is(err, exthost.ErrUnavailable) {
		t.Fatal(err)
	}
	await := func(pred func() bool) {
		t.Helper()
		for !pred() {
			select {
			case <-changed:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
	await(func() bool { return d.Health().Extensions["reader"].Status == "restarting" })
	clock.waitTimers(t, passPeriod, 3)
	clock.advance(2 * time.Second)
	await(func() bool { return d.Health().Extensions["reader"].Status == "ready" })
	checkErr(t, h.Call(ctx, "fixture/close", at, nil))
	checkErr(t, d.runtime.repo.Read(ctx, func(r engine.Reader) error {
		w, err := r.GetWork(work)
		if w.Ended == nil || w.Ended.Outcome != "done" {
			t.Fatal(w)
		}
		return err
	}))
	clock.waitTimers(t, passPeriod, 2)
	checkErr(t, os.WriteFile(manifest, []byte("{}"), 0600))
	clock.advance(2 * time.Second)
	select {
	case <-d.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	checkErr(t, d.Stop(ctx))
	for _, id := range []string{"reader", "second"} {
		if d.Health().Reports[id]["shutdown"].Status != "ok" {
			t.Fatalf("%s 未完成 shutdown", id)
		}
	}
	if _, err := d.db.Info(ctx); err == nil {
		t.Fatal("停止后库仍打开")
	}
	client := inboxDB(t, filepath.Join(stateDir, "craft.db"))
	checkErr(t, client.View(ctx, func(rd store.SQLiteReader) error {
		rows, err := rd.Lookup("stateGet", "reader", "shutdown")
		if len(rows) != 1 {
			t.Fatal(rows)
		}
		return err
	}))
}

func TestDaemonHostAlarmTimeout(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	root := t.TempDir()
	p := installTestManifest(t, root, "reader", "feed", 1, true)
	raw, err := os.ReadFile(p)
	checkErr(t, err)
	checkErr(t, os.WriteFile(p, []byte(strings.Replace(string(raw), `"id":"poll","every":"1h"`, `"id":"hold","every":"10s","timeout":"1s"`, 1)), 0600))
	clock := newTestClock()
	d := NewDaemon(DaemonConfig{StateDir: t.TempDir(), Clock: clock, ExtensionDirs: []string{root}})
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	checkErr(t, d.Start(ctx))
	t.Cleanup(func() { checkErr(t, d.Stop(context.Background())) })
	// 本用例逐拍检查扩展超时的退避定时器，隔离独立的内核周期任务。
	checkErr(t, d.alarms.Unregister("watch.sessions"))
	client := inboxDB(t, filepath.Join(d.cfg.StateDir, "craft.db"))
	id, err := EnqueueAlarm(ctx, client, "reader", "hold")
	checkErr(t, err)
	// 只等明确的完成状态，不用实际耗时推断对端是否已收到请求。
	await := func(pred func() bool) {
		t.Helper()
		for !pred() {
			if ctx.Err() != nil {
				t.Fatal(ctx.Err())
			}
			runtime.Gosched()
		}
	}
	clock.waitTimers(t, passPeriod, 2)
	clock.advance(passPeriod)
	await(func() bool { return d.Health().Reports["reader"]["entered"].Detail == "requested" })
	clock.waitTimers(t, passPeriod, 2)
	clock.advance(time.Second)
	await(func() bool {
		return d.Health().Reports["reader"]["canceled"].Status == "ok" && d.alarms.List()[0].ConsecutiveFailures == 1
	})
	clock.waitTimers(t, 2*time.Second, 1)
	clock.advance(time.Second)
	await(func() bool {
		r, err := ReadAlarmRequest(ctx, client, id)
		checkErr(t, err)
		return r.Status == "failed"
	})
	clock.advance(time.Second)
	await(func() bool { return d.alarms.List()[0].LastResult == "skipped" })
	if s := d.alarms.List()[0]; s.State != "running" {
		t.Fatal(s)
	}
	checkErr(t, d.hosts["reader"].Call(ctx, "fixture/release", struct{}{}, nil))
	await(func() bool { return d.alarms.List()[0].State != "running" })
	next := d.alarms.List()[0].Next.Sub(clock.Now())
	// 跳过本拍时已经登记过一次；释放名额会唤醒服务并重新登记。等新定时器后再拨钟。
	clock.waitTimers(t, next, 2)
	clock.advance(next)
	await(func() bool { return d.Health().Reports["reader"]["again"].Detail == "schedule" })
}

func TestDaemonPolicyRecoveryAndEmptyFingerprint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root, state := t.TempDir(), t.TempDir()
		p := installTestManifest(t, root, "future", "other", 2, false)
		db := inboxDB(t, filepath.Join(state, "craft.db"))
		r := testRuntime(t, db)
		checkErr(t, r.ApplyPolicy(t.Context(), engine.Settings{Paused: true, EvalTimeoutMs: 700}, nil))
		req := AlarmRequest{RequestID: "interrupted", Alarm: "future:poll", Status: "running"}
		checkErr(t, db.Write(t.Context(), func(tx *store.SQLiteTx) error { return req.save(tx) }))
		clock := &passClock{}
		d := NewDaemon(DaemonConfig{StateDir: state, Clock: clock, ExtensionDirs: []string{root}})
		checkErr(t, d.Start(t.Context()))
		defer func() { checkErr(t, d.Stop(t.Context())) }()
		if p := d.runtime.policy.Load(); !p.Paused() || p.EvalTimeoutMs() != 700 {
			t.Fatal("已保存的设置被丢弃")
		}
		got, err := ReadAlarmRequest(t.Context(), db, req.RequestID)
		checkErr(t, err)
		if got.Status != "failed" {
			t.Fatal(got)
		}
		checkErr(t, os.Remove(p))
		clock.advance(passPeriod)
		select {
		case <-d.Done():
			t.Fatal("空指纹触发退出")
		default:
		}
		checkErr(t, os.WriteFile(p, []byte("{}"), 0600))
		clock.advance(passPeriod)
		select {
		case <-d.Done():
		default:
			t.Fatal("变更指纹未触发退出")
		}
	})
}

func TestDaemonStopContinuesAfterErrors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()
		d := NewDaemon(DaemonConfig{StateDir: t.TempDir()})
		d.db = inboxDB(t, filepath.Join(d.cfg.StateDir, "db"))
		s, err := alarm.NewService(alarm.Config{Clock: d.cfg.Clock, GlobalLimit: 1})
		checkErr(t, err)
		d.alarms = s
		d.queue = newAlarmQueue(d.db, s)
		started, release := make(chan struct{}), make(chan struct{})
		checkErr(t, s.Register(alarm.AlarmSpec{ID: "slow", Every: time.Hour, Run: func(context.Context, alarm.RunRequest) error { close(started); <-release; return nil }}))
		checkErr(t, s.Start(ctx))
		checkErr(t, s.Trigger("slow"))
		<-started
		effectErr := errors.New("测试停止失败")
		layer := passEffects{start: func(context.Context) error { return nil }, kick: func() {}, stop: func(context.Context) error {
			close(release)
			if _, err := d.db.Info(ctx); err != nil {
				t.Error("过早关闭数据库", err)
			}
			return effectErr
		}}
		d.runtime, err = NewRuntime(d.db, d.cfg.Clock, benchScope{}, layer, nil)
		checkErr(t, err)
		checkErr(t, d.runtime.Start(ctx, engine.Settings{}, nil))
		err = d.shutdown()
		if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, effectErr) {
			t.Fatalf("未汇总两步失败: %v", err)
		}
		if _, err := d.db.Info(ctx); err == nil {
			t.Fatal("停止出错后库未关闭")
		}
	})
}
