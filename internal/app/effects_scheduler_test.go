package app

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/store"
)

// 虚拟时钟由同一测试内的 synctest 驱动；先等当前写回与定时器登记完，再拨到指定时刻。
type schedulerTestClock struct{ alarm.RealClock }

func (c schedulerTestClock) advance(d time.Duration) {
	synctest.Wait()
	<-c.NewTimer(d).C
	synctest.Wait()
}

type testEffectHandler struct {
	kind           string
	limit          atomic.Int32
	ordered, quiet bool
	run            func(context.Context, store.Effect) EffectResult
}

func (h *testEffectHandler) Kind() string  { return h.kind }
func (h *testEffectHandler) Limit() int    { return int(h.limit.Load()) }
func (h *testEffectHandler) Quiet() bool   { return h.quiet }
func (h *testEffectHandler) Ordered() bool { return h.ordered }
func (h *testEffectHandler) Run(ctx context.Context, e store.Effect) EffectResult {
	return h.run(ctx, e)
}

type effectCall struct {
	row    store.Effect
	finish chan EffectResult
}

func controlledHandler(kind string, limit int32) (*testEffectHandler, chan effectCall) {
	calls := make(chan effectCall, 16)
	h := &testEffectHandler{kind: kind}
	h.limit.Store(limit)
	h.run = func(ctx context.Context, e store.Effect) EffectResult {
		call := effectCall{e, make(chan EffectResult, 1)}
		select {
		case calls <- call:
		case <-ctx.Done():
			return EffectResult{}
		}
		select {
		case res := <-call.finish:
			return res
		case <-ctx.Done():
			return EffectResult{}
		}
	}
	return h, calls
}

func effectOK(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func effectDB(t testing.TB, now func() time.Time) *store.SQLite {
	t.Helper()
	db, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "effects.db"), nil, store.WithClock(now))
	effectOK(t, err)
	t.Cleanup(func() { effectOK(t, db.Close()) })
	return db
}

func enqueueEffects(t testing.TB, db *store.SQLite, kind string, keys ...string) {
	t.Helper()
	effectOK(t, db.Write(t.Context(), func(tx *store.SQLiteTx) error {
		for _, key := range keys {
			if err := tx.Enqueue(kind, key, []byte("1")); err != nil {
				return err
			}
		}
		return nil
	}))
}

func effectHead(t *testing.T, db *store.SQLite, kind string) store.Effect {
	t.Helper()
	var head *store.Effect
	effectOK(t, db.View(t.Context(), func(r store.SQLiteReader) error {
		var err error
		head, err = r.HeadEffect(kind)
		return err
	}))
	if head == nil {
		t.Fatal("队首缺失", kind)
	}
	return *head
}

type effectFixture struct {
	db     *store.SQLite
	clock  schedulerTestClock
	layer  EffectLayer
	mu     sync.Mutex
	health map[string]HealthState
}

func (f *effectFixture) report(kind string, state HealthState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health[kind] = state
}

func (f *effectFixture) state(kind string) HealthState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health[kind]
}

func newEffectFixture(t *testing.T, handlers ...EffectHandler) *effectFixture {
	t.Helper()
	f := &effectFixture{health: make(map[string]HealthState)}
	f.db = effectDB(t, f.clock.Now)
	layer, err := NewEffectScheduler(f.db, f.clock, time.Second, 3*time.Second, f.report, handlers...)
	effectOK(t, err)
	f.layer = layer
	t.Cleanup(func() { effectOK(t, f.layer.Stop(context.Background())) })
	return f
}

func takeCall(t *testing.T, calls chan effectCall) effectCall {
	t.Helper()
	synctest.Wait()
	select {
	case call := <-calls:
		return call
	default:
		t.Fatal("处理器未收到应执行的行")
		return effectCall{}
	}
}

func noCalls(t *testing.T, calls chan effectCall) {
	t.Helper()
	synctest.Wait()
	select {
	case call := <-calls:
		t.Fatalf("意外执行 %+v", call.row)
	default:
	}
}

func TestEffectLimitsAndConcurrentKick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, calls := controlledHandler("work", 0)
		f := newEffectFixture(t, h)
		enqueueEffects(t, f.db, h.kind, "a", "b", "c", "d")
		f.layer.Kick()
		noCalls(t, calls)
		effectOK(t, f.layer.Start(t.Context()))
		noCalls(t, calls)
		h.limit.Store(2)
		var kicks sync.WaitGroup
		for range 32 {
			kicks.Go(f.layer.Kick)
		}
		kicks.Wait()
		a, b := takeCall(t, calls), takeCall(t, calls)
		if a.row.Key == b.row.Key {
			t.Fatal("重复提交", a.row.Key)
		}
		noCalls(t, calls)
		// 降低上限不能中断已开始的处理器，也不能在名额尚满时补入新行。
		h.limit.Store(1)
		a.finish <- EffectResult{}
		noCalls(t, calls)
		b.finish <- EffectResult{}
		c := takeCall(t, calls)
		if c.row.Key != "c" {
			t.Fatal(c.row)
		}
		noCalls(t, calls)
		c.finish <- EffectResult{}
		d := takeCall(t, calls)
		if d.row.Key != "d" {
			t.Fatal(d.row)
		}
		d.finish <- EffectResult{}
		noCalls(t, calls)
		if s := f.state(h.kind); s.Waiting != 0 || s.Status != "ok" {
			t.Fatal(s)
		}
	})
}

func TestEffectOrderedRetryAndHealth(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		name := "report"
		if quiet {
			name = "quiet"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, calls := controlledHandler("ordered", 4)
				h.ordered, h.quiet = true, quiet
				f := newEffectFixture(t, h)
				enqueueEffects(t, f.db, h.kind, "first", "second")
				effectOK(t, f.layer.Start(t.Context()))
				call := takeCall(t, calls)
				message := strings.Repeat("界", 1001)
				for i, wait := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second, 3 * time.Second} {
					if call.row.Key != "first" || call.row.Attempts != i {
						t.Fatal(call.row)
					}
					noCalls(t, calls)
					call.finish <- EffectResult{Err: errors.New(message)}
					noCalls(t, calls)
					state := f.state(h.kind)
					if state.Waiting != 2 || state.Oldest != call.row.Since || state.MaxAttempts != i+1 || state.LastError != strings.Repeat("界", 1000) ||
						(state.Status == "error") != (i >= 2 && !quiet) {
						t.Fatalf("第 %d 次失败 %+v", i+1, state)
					}
					f.clock.advance(wait - time.Millisecond)
					noCalls(t, calls)
					f.clock.advance(time.Millisecond)
					call = takeCall(t, calls)
				}
				call.finish <- EffectResult{Later: true}
				f.clock.advance(time.Second - time.Millisecond)
				noCalls(t, calls)
				f.clock.advance(time.Millisecond)
				call = takeCall(t, calls)
				if call.row.Attempts != 4 || call.row.Key != "first" {
					t.Fatal(call.row)
				}
				call.finish <- EffectResult{}
				next := takeCall(t, calls)
				if next.row.Key != "second" || next.row.Seq <= call.row.Seq {
					t.Fatal(next.row)
				}
				next.finish <- EffectResult{}
				noCalls(t, calls)
				if s := f.state(h.kind); s.Waiting != 0 || s.Status != "ok" || s.LastError != "" {
					t.Fatal(s)
				}
			})
		})
	}
}

func TestEffectRunningReplacement(t *testing.T) {
	for name, result := range map[string]EffectResult{"ok": {}, "later": {Later: true}, "failed": {Err: errors.New("旧错误")}} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h, calls := controlledHandler("work", 2)
				f := newEffectFixture(t, h)
				enqueueEffects(t, f.db, h.kind, "a")
				effectOK(t, f.layer.Start(t.Context()))
				old := takeCall(t, calls)
				// 同键新代次仍占原位置；另一个键必须能越过执行中的旧代次使用空槽。
				enqueueEffects(t, f.db, h.kind, "a", "b")
				f.layer.Kick()
				b := takeCall(t, calls)
				if b.row.Key != "b" {
					t.Fatal(b.row)
				}
				f.layer.Kick()
				noCalls(t, calls)
				b.finish <- EffectResult{}
				noCalls(t, calls)
				old.finish <- result
				fresh := takeCall(t, calls)
				if fresh.row.Key != "a" || fresh.row.Generation != old.row.Generation+1 || fresh.row.Seq != old.row.Seq ||
					fresh.row.Attempts != 0 || fresh.row.Due != old.row.Due || fresh.row.Error != "" {
					t.Fatal(fresh.row)
				}
				fresh.finish <- EffectResult{}
				noCalls(t, calls)
			})
		})
	}
}

func TestEffectPanicAndDispatchIsolation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bad, badCalls := controlledHandler("bad", 1)
		bad.ordered, bad.quiet = true, true
		good, calls := controlledHandler("good", 1)
		h := &testEffectHandler{kind: "panic"}
		h.limit.Store(1)
		h.run = func(_ context.Context, e store.Effect) EffectResult {
			if e.Attempts == 0 {
				panic("运行中断")
			}
			return EffectResult{}
		}
		f := newEffectFixture(t, bad, h, good)
		for _, kind := range []string{bad.kind, h.kind, good.kind} {
			enqueueEffects(t, f.db, kind, "one")
		}
		row := effectHead(t, f.db, bad.kind)
		// 四位年份之外无法编码为队列时间，只让该类队首的调度读取失败。
		effectOK(t, f.db.Write(t.Context(), func(tx *store.SQLiteTx) error {
			return tx.LaterEffect(row, time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), 0)
		}))
		effectOK(t, f.layer.Start(t.Context()))
		call := takeCall(t, calls)
		call.finish <- EffectResult{}
		synctest.Wait()
		if s := f.state(bad.kind); s.Status != "error" || s.LastError == "" || s.Waiting != 1 {
			t.Fatal(s)
		}
		if s := f.state(h.kind); s.MaxAttempts != 1 || !strings.Contains(s.LastError, "运行中断") {
			t.Fatal(s)
		}
		if s := f.state(good.kind); s.Status != "ok" || s.Waiting != 0 {
			t.Fatal(s)
		}
		enqueueEffects(t, f.db, bad.kind, "one")
		f.layer.Kick()
		repaired := takeCall(t, badCalls)
		repaired.finish <- EffectResult{}
		noCalls(t, badCalls)
		if s := f.state(bad.kind); s.Status != "ok" || s.Waiting != 0 {
			t.Fatal(s)
		}
		f.clock.advance(time.Second)
		if s := f.state(h.kind); s.Waiting != 0 || s.LastError != "" {
			t.Fatal(s)
		}
	})
}

func TestEffectStopAndRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, calls := controlledHandler("stop", 1)
		cancelled, release := make(chan struct{}), make(chan struct{})
		signalCancellation := sync.OnceFunc(func() { close(cancelled) })
		unblock := sync.OnceFunc(func() { close(release) })
		run := h.run
		h.run = func(ctx context.Context, e store.Effect) EffectResult {
			r := run(ctx, e)
			if ctx.Err() != nil {
				signalCancellation()
				<-release
			}
			return r
		}
		f := newEffectFixture(t, h)
		t.Cleanup(unblock)
		enqueueEffects(t, f.db, h.kind, "one", "two")
		effectOK(t, f.layer.Stop(t.Context()))
		effectOK(t, f.layer.Start(t.Context()))
		effectOK(t, f.layer.Start(t.Context()))
		old := takeCall(t, calls)
		deadline, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.layer.Stop(deadline) }()
		synctest.Wait()
		select {
		case <-cancelled:
		default:
			t.Fatal("处理器未收到取消信号")
		}
		f.layer.Kick()
		noCalls(t, calls)
		f.clock.advance(time.Second)
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		default:
			t.Fatal("停止超过了期限")
		}
		if err := f.layer.Start(t.Context()); err == nil {
			t.Fatal("仍在停止时重复启动")
		}
		unblock()
		effectOK(t, f.layer.Stop(t.Context()))
		f.layer.Kick()
		noCalls(t, calls)
		if got := effectHead(t, f.db, h.kind); !reflect.DeepEqual(got, old.row) {
			t.Fatalf("停止改动了队首 %+v", got)
		}
		effectOK(t, f.layer.Start(t.Context()))
		fresh := takeCall(t, calls)
		if !reflect.DeepEqual(fresh.row, old.row) {
			t.Fatal(fresh.row)
		}
		fresh.finish <- EffectResult{}
		second := takeCall(t, calls)
		second.finish <- EffectResult{}
		noCalls(t, calls)
	})
}

func TestEffectUnorderedWakesWithRunningKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, calls := controlledHandler("parallel", 2)
		other, otherCalls := controlledHandler("other", 1)
		f := newEffectFixture(t, h, other)
		delay := func(kind, key string, d time.Duration) {
			t.Helper()
			enqueueEffects(t, f.db, kind, key)
			row := effectHead(t, f.db, kind)
			effectOK(t, f.db.Write(t.Context(), func(tx *store.SQLiteTx) error {
				return tx.LaterEffect(row, f.clock.Now(), d)
			}))
		}
		delay(h.kind, "future", time.Hour)
		enqueueEffects(t, f.db, h.kind, "running")
		effectOK(t, f.layer.Start(t.Context()))
		first := takeCall(t, calls)
		if first.row.Key != "running" {
			t.Fatal(first.row)
		}
		delay(other.kind, "earlier", 2*time.Second)
		f.layer.Kick()
		f.clock.advance(2*time.Second - time.Millisecond)
		noCalls(t, otherCalls)
		f.clock.advance(time.Millisecond)
		earlier := takeCall(t, otherCalls)
		earlier.finish <- EffectResult{}
		noCalls(t, calls)
		f.clock.advance(time.Hour - 2*time.Second)
		future := takeCall(t, calls)
		if future.row.Key != "future" {
			t.Fatal(future.row)
		}
		future.finish <- EffectResult{}
		first.finish <- EffectResult{}
		noCalls(t, calls)
	})
}

// 处理器可以给出没有业务限制的名额，队列长度仍不能决定 goroutine 数量。
func TestEffectWorkerBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := &testEffectHandler{kind: "unlimited"}
		h.limit.Store(math.MaxInt32)
		var started atomic.Int32
		release := make(chan struct{})
		h.run = func(ctx context.Context, _ store.Effect) EffectResult {
			started.Add(1)
			select {
			case <-ctx.Done():
			case <-release:
			}
			return EffectResult{}
		}
		f := newEffectFixture(t, h)
		const count = 128
		var keys []string
		for i := range count {
			keys = append(keys, strconv.Itoa(i))
		}
		enqueueEffects(t, f.db, h.kind, keys...)
		effectOK(t, f.layer.Start(t.Context()))
		synctest.Wait()
		if n := started.Load(); n == 0 || n >= count {
			t.Fatalf("无业务限额时同时执行了 %d 行", n)
		}
		close(release)
		synctest.Wait()
		if n := started.Load(); n != count || f.state(h.kind).Waiting != 0 {
			t.Fatalf("只消费了 %d 行", n)
		}
	})
}

type dispatchProbe struct {
	*testEffectHandler
	checks atomic.Int32
}

func (h *dispatchProbe) Limit() int { h.checks.Add(1); return h.testEffectHandler.Limit() }

func TestEffectIdleWaitsForKick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler, calls := controlledHandler("empty", 2)
		h := &dispatchProbe{testEffectHandler: handler}
		f := newEffectFixture(t, h)
		effectOK(t, f.layer.Start(t.Context()))
		noCalls(t, calls)
		checks := h.checks.Load()
		f.clock.advance(24 * time.Hour)
		if checks == 0 || h.checks.Load() != checks {
			t.Fatal("空队列发生轮询")
		}
		enqueueEffects(t, f.db, h.kind, "one")
		f.layer.Kick()
		call := takeCall(t, calls)
		checks = h.checks.Load()
		f.clock.advance(24 * time.Hour)
		if h.checks.Load() != checks {
			t.Fatal("正在执行的行造成轮询")
		}
		call.finish <- EffectResult{}
		noCalls(t, calls)
	})
}
