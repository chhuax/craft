package enginetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

type itemScope struct{ scope }

func (itemScope) Closable(_ string, kind string) bool { return kind == "item" }

func evalPolicy(t *testing.T, settings engine.Settings) engine.Policy {
	t.Helper()
	p, err := engine.NewPolicy(settings, []engine.EventDef{
		{Source: "feed", Event: "assign", Strength: engine.Explicit, Default: engine.ModeAuto},
		{Source: "feed", Event: "signal", Default: engine.ModeAuto},
		{Source: "feed", Event: "watch", Strength: engine.Explicit, Default: engine.ModeObserve},
		{Source: "feed", Event: "off", Default: engine.ModeOff},
	})
	must(t, err)
	return p
}

func evalEngine(t *testing.T, s engine.Store, p *engine.Policy) *engine.Engine {
	t.Helper()
	cfg := config(s)
	cfg.Policy = func() engine.Policy { return *p }
	cfg.Scope = itemScope{}
	return newEngine(t, cfg)
}

func item(id string) engine.ThreadRef { return engine.NewThreadRef("feed", "item", id) }

func getThread(t *testing.T, s engine.Store, ref engine.ThreadRef) engine.Thread {
	t.Helper()
	var th *engine.Thread
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		var err error
		th, err = r.GetThread(engine.ThreadKey(ref))
		return err
	}))
	if th == nil {
		t.Fatalf("缺少对象 %v", ref)
	}
	return *th
}

func checkDue(t *testing.T, e *engine.Engine, want int) {
	t.Helper()
	n, err := e.EvaluateDue(t.Context())
	must(t, err)
	if n != want {
		t.Fatalf("处理 %d 个对象，期望 %d", n, want)
	}
}

func held(t *testing.T, s engine.Store, ref engine.ThreadRef) engine.Evaluation {
	t.Helper()
	th := getThread(t, s, ref)
	if th.Evaluating == nil {
		t.Fatalf("缺少研判 %+v", th)
	}
	return *th.Evaluating
}

func checkCode(t *testing.T, err error, code string) {
	t.Helper()
	var rejection *engine.UserError
	if !errors.As(err, &rejection) || rejection.Code != code {
		t.Fatalf("得到 %v，期望 %q", err, code)
	}
}

func evaluationBatch(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("queue")
	for _, tc := range []struct{ id, event, at string }{
		{"one", "assign", "2025-04-01"}, {"two", "assign", "2025-04-03"}, {"three", "assign", "2025-04-02"},
		{"four", "signal", "2025-04-04"}, {"five", "off", "2025-04-05"},
	} {
		tr := trigger(tc.id, tc.event, ref)
		tr.At = tc.at + "T00:00:00Z"
		tr.Actor.ID = tc.id
		receive(t, e, tr, engine.Stored)
	}
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	if ev.Primary != "two" || ev.Actor.ID != "two" || ev.Strength != engine.Explicit || ev.PolicyVersion != p.Version() || ev.Basis.Via != engine.ViaEvent ||
		!slices.Equal(ev.TriggerIDs, []string{"one", "two", "three", "four", "five"}) {
		t.Fatalf("研判批次 %+v", ev)
	}
	receive(t, e, trigger("six", "signal", ref), engine.Stored)
	checkDue(t, e, 0)
	ok, err := e.Reevaluate(t.Context(), "one")
	must(t, err)
	if ok || !slices.Equal(getThread(t, s, ref).Pending, []string{"six"}) {
		t.Fatal("进行中的研判被打断")
	}
	changed, err := e.ReassessEval(t.Context(), ev.ID)
	must(t, err)
	if !changed || !slices.Equal(getThread(t, s, ref).Pending, append(slices.Clone(ev.TriggerIDs), "six")) {
		t.Fatal("未按原顺序重排")
	}
	changed, err = e.ReassessEval(t.Context(), ev.ID)
	must(t, err)
	if changed {
		t.Fatal("重复重排改变了队列")
	}
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		ds, err := r.DecisionsByThread(ev.ThreadKey)
		must(t, err)
		if len(ds) != 0 {
			t.Fatal("应用层尚未判定最终失败，引擎已记录不可用决策")
		}
		return nil
	}))
	checkDue(t, e, 1)
	next := held(t, s, ref)
	if next.ID == ev.ID || len(next.TriggerIDs) != 6 {
		t.Fatal("新旧批次未隔离")
	}
	changed, err = e.ReassessEval(t.Context(), next.ID)
	must(t, err)
	checkDue(t, e, 1)
	if !changed || held(t, s, ref).ID == next.ID {
		t.Fatal("同毫秒重试相同事件复用了旧代次")
	}
}

func evaluationPaused(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{Paused: true})
	e := evalEngine(t, s, &p)
	a := item("mixed")
	receive(t, e, trigger("automation", "assign", a), engine.Stored)
	req := trigger("request", "person.request", a)
	req.Source = "craft"
	req.Human = true
	req.Addressed = "direct"
	receive(t, e, req, engine.Stored)
	receive(t, e, trigger("other", "signal", item("auto")), engine.Stored)
	checkDue(t, e, 1)
	ev := held(t, s, a)
	if !slices.Equal(ev.TriggerIDs, []string{"request"}) || !slices.Equal(getThread(t, s, a).Pending, []string{"automation"}) {
		t.Fatal("暂停期间消费了其他事件")
	}
	checkDue(t, e, 0)
}

func evaluationPreflight(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("preflight")
	for _, pair := range [][2]string{{"owned", "assign"}, {"started", "signal"}, {"denied", "off"}, {"absent", "unknown"}} {
		tr := trigger(pair[0], pair[1], ref)
		tr.Human = true
		receive(t, e, tr, engine.Stored)
	}
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutWork(engine.Work{ID: "worker"}))
		must(t, u.PutDisposition("owned", engine.TriggerDisposition{Fanout: []string{"worker"}}))
		return u.PutDecision(engine.Decision{ID: "prior", ThreadKey: engine.ThreadKey(ref), Kind: engine.Accept, WorkID: "ended", TriggerIDs: []string{"started"}})
	}))
	checkDue(t, e, 1)
	th := getThread(t, s, ref)
	if th.Evaluating != nil || len(th.Pending) != 0 {
		t.Fatal("预检没有清空队列")
	}
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		for id, code := range map[string]string{"denied": engine.ReasonModeOff, "absent": engine.ReasonUnknownEvent, "started": engine.ReasonStartedWork} {
			disp, err := r.GetDisposition(id)
			must(t, err)
			d, err := r.GetDecision(disp.DecisionID)
			must(t, err)
			if d == nil || d.Kind != engine.Ignore || !slices.ContainsFunc(d.Reason, func(v engine.Reason) bool { return v.TriggerID == id && v.Code == code }) {
				t.Fatalf("事件 %s 决策 %+v", id, d)
			}
		}
		events, err := r.EventsSince(0, 30)
		must(t, err)
		if !slices.ContainsFunc(events, func(v engine.Event) bool { return v.Type == "trigger.followup" && v.Ref["trigger"] == "owned" }) {
			t.Fatal("跟进未通知")
		}
		return nil
	}))
}

type queueStore struct {
	engine.Store
	check  func(string, string, json.RawMessage) error
	before func()
}
type queueUnit struct {
	engine.Unit
	store *queueStore
}

func (s *queueStore) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	if s.before != nil {
		s.before()
	}
	return s.Store.Atomic(ctx, func(u engine.Unit) error { return fn(queueUnit{u, s}) })
}
func (u queueUnit) Effects() engine.Effects { return u }
func (u queueUnit) Enqueue(kind, key string, value json.RawMessage) error {
	if err := u.store.check(kind, key, value); err != nil {
		return err
	}
	return u.Unit.Effects().Enqueue(kind, key, value)
}

func independentEvals(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	calls := 0
	faults := []error{errors.New("首个对象故障"), errors.New("另一个对象故障")}
	store := &queueStore{Store: s, check: func(kind, key string, v json.RawMessage) error {
		calls++
		var payload struct{ EvalID string }
		must(t, json.Unmarshal(v, &payload))
		if kind != "assessment" || payload.EvalID != key {
			t.Fatalf("入队 %s %s %s", kind, key, v)
		}
		if calls < 3 {
			return fmt.Errorf("第 %d 次: %w", calls, faults[calls-1])
		}
		return nil
	}}
	snapshots := 0
	cfg := config(store)
	cfg.Scope = itemScope{}
	cfg.Policy = func() engine.Policy { snapshots++; return p }
	e := newEngine(t, cfg)
	for _, id := range []string{"a", "b", "c", "closed"} {
		receive(t, e, trigger(id, "assign", item(id)), engine.Stored)
	}
	store.before = func() {
		store.before = nil
		other := evalEngine(t, s, &p)
		must(t, other.ThreadClosed(t.Context(), item("closed"), "done", now, ""))
	}
	snapshots = 0
	n, err := e.EvaluateDue(t.Context())
	if snapshots != 1 {
		t.Fatalf("一轮 Pass 取策略 %d 次", snapshots)
	}
	if n != 1 || !errors.Is(err, faults[0]) || !errors.Is(err, faults[1]) || calls != 3 {
		t.Fatalf("独立提交 %d %v %d", n, err, calls)
	}
	for _, id := range []string{"a", "b", "closed"} {
		th := getThread(t, s, item(id))
		if !slices.Equal(th.Pending, []string{id}) || th.Evaluating != nil {
			t.Fatalf("失败或已关闭的对象被推进 %+v", th)
		}
	}
	if held(t, s, item("c")).Primary != "c" {
		t.Fatal("失败影响了其他对象")
	}
}
