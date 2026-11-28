package enginetest

import (
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func propagateClose(t *testing.T, s engine.Store) {
	ctx := t.Context()
	cfg := config(s)
	snapshots := 0
	cfg.Policy = func() engine.Policy { snapshots++; return engine.Policy{} }
	e := newEngine(t, cfg)
	one, two, third := item("one"), item("two"), item("third")
	primary, second := engine.ThreadKey(one), engine.ThreadKey(two)
	ids := []string{"owner", "peer", "context"}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutThread(engine.Thread{Key: primary, Ref: one, Subscriptions: ids, Context: ids[2:]}))
		must(t, u.PutThread(engine.Thread{Key: second, Ref: two, Subscriptions: ids[:1]}))
		must(t, u.PutThread(engine.Thread{Key: engine.ThreadKey(third), Ref: third, Subscriptions: ids[1:2]}))
		must(t, u.PutWork(engine.Work{ID: "owner", Execution: &engine.Execution{Session: &engine.Session{Host: "local", ID: "live"}}, Follows: []engine.Follow{{Thread: primary, Own: true}, {Thread: second, Own: true}}, Assignments: []engine.Assignment{{ID: "a", State: engine.AssignmentRunning, Report: &engine.Report{State: engine.Ready, At: now}}}}))
		must(t, u.PutWork(engine.Work{ID: "peer", Follows: []engine.Follow{{Thread: primary}, {Thread: engine.ThreadKey(third), Own: true}}}))
		return u.PutWork(engine.Work{ID: "context"})
	}))
	const early = "2026-01-01T00:00:00.000Z"
	const mid = "2026-01-01T01:00:00.000Z"
	must(t, e.ThreadClosed(ctx, one, "closed", early, "第一次关闭"))
	for _, id := range ids[:2] {
		w := readWork(t, s, id)
		if w.Ended != nil || w.Follows[0].Closed == nil || w.Follows[0].Closed.Detail != "第一次关闭" {
			t.Fatalf("部分关闭 %+v", w)
		}
	}
	must(t, e.ThreadReopened(ctx, one, mid))
	must(t, e.ThreadClosed(ctx, one, "closed", early, "迟到"))
	for _, id := range ids[:2] {
		if w := readWork(t, s, id); w.Follows[0].Closed != nil || w.Ended != nil {
			t.Fatalf("重开或旧关闭影响错误 %+v", w)
		}
	}
	must(t, e.ThreadClosed(ctx, two, "closed", mid, ""))
	must(t, e.ThreadClosed(ctx, third, "closed", mid, ""))
	if w := readWork(t, s, "peer"); w.Ended == nil || w.Ended.Outcome != engine.Closed || w.Ended.At != mid {
		t.Fatal("非自有的未关对象妨碍结束", w)
	}
	must(t, e.ThreadClosed(ctx, one, "done", now, ""))
	if w := readWork(t, s, "owner"); w.Ended == nil || w.Ended.Outcome != engine.Done || w.Ended.By != "executor" || w.Ended.At != now {
		t.Fatalf("最后一个自有对象关闭 %+v", w)
	}
	if !slices.Equal(getThread(t, s, one).Subscriptions, []string{"context"}) || len(getThread(t, s, two).Subscriptions) != 0 || readWork(t, s, "context").Ended != nil {
		t.Fatal("结束联动影响了旁观者")
	}
	must(t, e.ThreadReopened(ctx, one, "2026-01-03T00:00:00Z"))
	if readWork(t, s, "owner").Ended == nil || snapshots != 7 {
		t.Fatalf("重开恢复已结束工作或策略快照次数 %d", snapshots)
	}
}

func reboundClose(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("rebound")
	must(t, e.ThreadClosed(ctx, ref, "done", now, ""))
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(engine.Work{ID: "w"}) }))
	w, err := e.BindThread(ctx, "w", ref, true)
	must(t, err)
	if w.Follows[0].Closed != nil {
		t.Fatal("手工绑定时未开始新的跟随")
	}
	must(t, e.ThreadClosed(ctx, ref, "done", now, ""))
	if readWork(t, s, "w").Ended == nil {
		t.Fatal("重放有效关闭时新绑定未得到事实")
	}
	must(t, e.ThreadReopened(ctx, ref, now))
	if getThread(t, s, ref).Closed == nil {
		t.Fatal("同一事实时刻的重开覆盖关闭")
	}
}

func closeWithoutSession(t *testing.T, s engine.Store) {
	ctx := t.Context()
	cfg := config(s)
	e := newEngine(t, cfg)
	r := item("invalid")
	key := engine.ThreadKey(r)
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutThread(engine.Thread{Key: key, Ref: r, Subscriptions: []string{"running"}}))
		return u.PutWork(engine.Work{ID: "running", Follows: []engine.Follow{{Thread: key, Own: true}}, Assignments: []engine.Assignment{{ID: "run", State: engine.AssignmentRunning}}})
	}))
	// 正常 launch 先登记会话才标记 running。损坏的记录不能提交一个无法定位会话的 stop。
	if err := e.ThreadClosed(ctx, r, "done", now, ""); err == nil {
		t.Fatal("缺失会话仍提交了停止请求")
	}
	if getThread(t, s, r).Closed != nil || readWork(t, s, "running").Ended != nil {
		t.Fatal("关闭联动的对象写入未回滚")
	}
}

func closeWithPriorOwnFact(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	own, side := item("finished"), item("side")
	const before = "2026-01-01T00:00:00.000Z"
	fact := &engine.ThreadClose{At: before, Outcome: "done"}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutThread(engine.Thread{Key: engine.ThreadKey(own), Ref: own, Closed: fact, Subscriptions: []string{"w"}}))
		must(t, u.PutThread(engine.Thread{Key: engine.ThreadKey(side), Ref: side, Subscriptions: []string{"w"}}))
		return u.PutWork(engine.Work{ID: "w", Follows: []engine.Follow{{Thread: engine.ThreadKey(own), Own: true, Closed: fact}, {Thread: engine.ThreadKey(side)}}})
	}))
	must(t, e.ThreadClosed(ctx, side, "closed", now, "旁观对象关闭"))
	w := readWork(t, s, "w")
	if w.Ended == nil || w.Ended.Outcome != engine.Done || w.Ended.At != before {
		t.Fatalf("未使用已有自有对象的结束事实 %+v", w.Ended)
	}
}
