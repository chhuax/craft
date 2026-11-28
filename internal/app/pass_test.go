package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

func testRuntime(t testing.TB, db *store.SQLite) *Runtime {
	t.Helper()
	r, err := NewRuntime(db, alarm.RealClock{}, benchScope{}, nil, nil)
	checkErr(t, err)
	checkErr(t, r.ApplyPolicy(t.Context(), engine.Settings{}, nil))
	return r
}

func TestDrainRollbackAndPoison(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		drain                func(*Runtime, context.Context) (bool, error)
		remaining, committed int
	}{{"逐行提交", (*Runtime).drain, 1, 1}, {"批事务", drainBatch, 4, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			db := inboxDB(t, filepath.Join(t.TempDir(), "db"))
			r := testRuntime(t, db)
			ctx := t.Context()
			src := testSource(t, db, r.Request)
			for _, id := range []string{"first", "bad"} {
				_, err := src.Receive(ctx, inboxTrigger(id))
				checkErr(t, err)
			}
			checkErr(t, db.Write(ctx, func(tx *store.SQLiteTx) error {
				// 解码关联对象发生在写入 Trigger 之后，可检测坏行是否遗留局部写入。
				if err := tx.Put("watch_thread", "feed:item:bad", []byte(`{"pending":42}`)); err != nil {
					return err
				}
				if _, err := tx.InsertInbox("malformed", []byte(`{"threads":2}`), benchAt); err != nil {
					return err
				}
				u := r.repo.in(ctx, tx)
				ref := engine.NewThreadRef("feed", "item", "last")
				if err := u.PutThread(engine.Thread{Key: engine.ThreadKey(ref), Ref: ref, Subscriptions: []string{"work"}}); err != nil {
					return err
				}
				if err := u.PutWork(engine.Work{ID: "work"}); err != nil {
					return err
				}
				// 最后一行才有订阅者；在保存 Work 后注入共享队列故障，不能按坏行删除。
				return tx.Put("watch_kv", "effect.seq", []byte(`"broken"`))
			}))
			_, err := src.Receive(ctx, inboxTrigger("last"))
			checkErr(t, err)
			var events []string
			r.repo.Subscribe(func(ev engine.Event) { events = append(events, ev.Ref["trigger"]) })
			if _, err := tc.drain(r, ctx); err == nil {
				t.Fatal("未报告队列故障")
			}
			checkErr(t, db.View(ctx, func(read store.SQLiteReader) error {
				rows, err := read.InboxBatch(10)
				if err != nil {
					return err
				}
				triggers, err := read.List("watch_trigger", store.All)
				if err != nil {
					return err
				}
				w, err := (engineReader{read}).GetWork("work")
				if err != nil {
					return err
				}
				if len(rows) != tc.remaining || len(events) != tc.committed || len(triggers) != tc.committed || triggers["bad"] != nil || triggers["last"] != nil || len(w.Inputs) != 0 {
					t.Fatalf("失败后 inbox=%v，Trigger=%v，广播=%v，输入=%v", rows, triggers, events, w.Inputs)
				}
				return nil
			}))
			checkErr(t, db.Write(ctx, func(tx *store.SQLiteTx) error { return tx.Delete("watch_kv", "effect.seq") }))
			more, err := tc.drain(r, ctx)
			checkErr(t, err)
			if more || !slices.Equal(events, []string{"first", "last"}) {
				t.Fatalf("重试后 %t, %v", more, events)
			}
			checkErr(t, db.View(ctx, func(read store.SQLiteReader) error {
				pending, err := read.HasInbox()
				if err != nil {
					return err
				}
				w, err := (engineReader{read}).GetWork("work")
				if err != nil {
					return err
				}
				if pending || len(w.Inputs) != 1 || w.Inputs[0].TriggerID != "last" {
					t.Fatalf("重试后 inbox=%t，输入=%v", pending, w.Inputs)
				}
				return nil
			}))
			got, err := src.Receive(ctx, inboxTrigger("first"))
			if err != nil || got != "duplicate" {
				t.Fatalf("重放 %q, %v", got, err)
			}
		})
	}
}

func TestReceiveDuringDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	r := testRuntime(t, inboxDB(t, path))
	const n = 40
	var counts [n]atomic.Int32
	var group sync.WaitGroup
	start := make(chan struct{})
	for range 4 {
		s := testSource(t, inboxDB(t, path), r.Request)
		group.Go(func() {
			<-start
			for round := range 2 {
				for i := range n {
					id := (i + round*7) % n
					result, err := s.Receive(t.Context(), inboxTrigger(fmt.Sprint(id)))
					if err != nil {
						t.Error(err)
						return
					}
					if result == "queued" {
						counts[id].Add(1)
					} else if result != "duplicate" {
						t.Error(result)
					}
				}
			}
		})
	}
	done := make(chan struct{})
	groupDone := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-groupDone:
				return
			case <-r.wake:
				if _, err := r.drain(t.Context()); err != nil {
					t.Error(err)
					return
				}
			}
		}
	}()
	close(start)
	group.Wait()
	close(groupDone)
	<-done
	_, err := r.drain(t.Context())
	checkErr(t, err)
	for i := range counts {
		if counts[i].Load() != 1 {
			t.Fatalf("事件 %d 入队 %d 次", i, counts[i].Load())
		}
	}
	checkErr(t, r.repo.Read(t.Context(), func(read engine.Reader) error {
		events, err := read.EventsSince(0, 2*n)
		if len(events) != n {
			t.Fatalf("已提交事件 %d", len(events))
		}
		return err
	}))
}

// 虚拟时间推进前先等循环重新进入等待，避免与定时器登记争用。
type passClock struct {
	alarm.RealClock
	period atomic.Int64
}

func (c *passClock) NewTimer(d time.Duration) *time.Timer {
	c.period.Store(int64(d))
	return c.RealClock.NewTimer(d)
}

func (c *passClock) advance(d time.Duration) {
	synctest.Wait()
	<-c.RealClock.NewTimer(d).C
	synctest.Wait()
}

type passEffects struct {
	start func(context.Context) error
	kick  func()
	stop  func(context.Context) error
}

func (e passEffects) Start(ctx context.Context) error { return e.start(ctx) }
func (e passEffects) Kick()                           { e.kick() }
func (e passEffects) Stop(ctx context.Context) error  { return e.stop(ctx) }

func TestPassWakeAndOrder(t *testing.T) { synctest.Test(t, testPassWakeAndOrder) }

func testPassWakeAndOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, external := inboxDB(t, path), inboxDB(t, path)
	ctx := t.Context()
	started := false
	var kicks atomic.Int32
	clock := &passClock{}
	hold := make(chan struct{})
	kickDone := make(chan struct{})
	stopCtx, cancelStop := context.WithCancel(ctx)
	defer cancelStop()
	var block atomic.Bool
	effects := passEffects{
		start: func(context.Context) error { started = true; return nil },
		kick: func() {
			if !started {
				t.Error("Pass 早于 effect 启动")
			}
			kicks.Add(1)
			if block.Load() {
				<-hold
				close(kickDone)
			}
		},
		stop: func(ctx context.Context) error {
			if _, err := db.Info(ctx); err != nil {
				t.Errorf("effect 停止前已关闭库: %v", err)
			}
			select {
			case <-kickDone:
			default:
				t.Error("Pass 尚未完成")
			}
			return nil
		},
	}
	alarmsStopped := make(chan struct{})
	r, err := NewRuntime(db, clock, benchScope{}, effects, func(context.Context) error { close(alarmsStopped); cancelStop(); return nil })
	checkErr(t, err)
	checkErr(t, r.Start(ctx, engine.Settings{}, nil))
	expect := func(n int) {
		t.Helper()
		synctest.Wait()
		if got := int(kicks.Load()); got != n {
			t.Fatalf("已运行 %d 轮，期望 %d", got, n)
		}
	}
	expect(1)
	if got := time.Duration(clock.period.Load()); got != 2*time.Second {
		t.Fatalf("等待周期 %v", got)
	}
	// 同进程命令提交后立即唤醒；失败的命令不留下唤醒。
	checkErr(t, r.Commands.DescribeThread(ctx, benchRef(0), nil, nil, nil))
	expect(2)
	err = r.Commands.PatchThreadMeta(ctx, "x", map[string]json.RawMessage{"facts": nil})
	if err == nil {
		t.Fatal("预期命令失败")
	}
	expect(2)
	// 第二条物理连接模拟 CLI。定时兜底运行一次，版本变化再立即运行一轮。
	checkErr(t, external.Write(ctx, func(tx *store.SQLiteTx) error { return tx.Put("watch_kv", "probe", []byte("1")) }))
	clock.advance(2*time.Second - time.Millisecond)
	expect(2)
	clock.advance(time.Millisecond)
	expect(4)
	// 只有时间变化时也应运行一轮。阻塞 Kick 后停止，检查当前轮被等待。
	block.Store(true)
	clock.advance(2 * time.Second)
	expect(5)
	if err := r.Stop(stopCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("当前轮仍在执行时停止返回 %v", err)
	}
	select {
	case <-alarmsStopped:
	default:
		t.Fatal("等待 Pass 前未停止闹钟")
	}
	if _, err := db.Info(ctx); err != nil {
		t.Fatalf("仍在运行的 Pass 失去数据库: %v", err)
	}
	close(hold)
	checkErr(t, r.Stop(ctx))
	if _, err := db.Info(ctx); err == nil {
		t.Fatal("停止后库仍然打开")
	}
}

func TestPassDrainsBacklogBeforeKick(t *testing.T) { synctest.Test(t, testPassDrainsBacklogBeforeKick) }

func testPassDrainsBacklogBeforeKick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := inboxDB(t, path)
	ctx := t.Context()
	// 在启动前积压 101 条；暂停的时钟下，最后一条只能由上一轮的余量唤醒来处理。
	clock := &passClock{}
	type observed struct {
		inbox      int
		assessment bool
	}
	var seen []observed
	effects := passEffects{start: func(context.Context) error { return nil }, stop: func(context.Context) error { return nil }, kick: func() {
		err := db.View(ctx, func(tx store.SQLiteReader) error {
			rows, err := tx.InboxBatch(101)
			if err != nil {
				return err
			}
			head, err := tx.HeadEffect("assessment")
			if err != nil {
				return err
			}
			seen = append(seen, observed{len(rows), head != nil})
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	}}
	r, err := NewRuntime(db, clock, benchScope{}, effects, nil)
	checkErr(t, err)
	src := testSource(t, db, r.Request)
	for i := range 101 {
		p := inboxTrigger(fmt.Sprint(i))
		p.Threads = p.Threads[:1]
		p.Threads[0].ID = "primary"
		_, err := src.Receive(ctx, p)
		checkErr(t, err)
	}
	checkErr(t, r.Start(ctx, engine.Settings{}, []engine.EventDef{{Source: "feed", Event: "change", Default: engine.ModeAuto}}))
	synctest.Wait()
	checkErr(t, r.Stop(ctx))
	if !slices.Equal(seen, []observed{{1, true}, {0, true}}) {
		t.Fatalf("各轮 Kick 时 inbox 余量与研判入队状态 %v", seen)
	}
}
