package enginetest

import (
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func attachBatch(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref, side := item("route"), item("side")
	key := engine.ThreadKey(ref)
	for _, id := range []string{"one", "two"} {
		tr := trigger(id, "signal", ref, side)
		tr.Human = true
		receive(t, e, tr, engine.Stored)
	}
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	grant := engine.Mandate{Strength: engine.Explicit, Scope: "原授权", PolicyVersion: "original", Basis: engine.MandateBasis{Source: "feed", Event: "assign"}}
	stale := &engine.ThreadClose{At: now, Outcome: "done"}
	w := engine.Work{ID: "target", Mandate: grant, Ended: &engine.WorkEnd{By: "executor"}, Follows: []engine.Follow{{Thread: key, Manual: &engine.ManualFollow{}, Closed: stale}}}
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(w) }))
	result, err := e.Attach(ctx, w.ID, "one", "一起处理")
	must(t, err)
	if result.Ended != nil || !reflect.DeepEqual(result.Mandate, grant) || len(result.Inputs) != 2 || result.Inputs[0].Note != "一起处理" ||
		result.Inputs[1].Kind != "resumed" || !slices.Equal(result.Undelivered, []string{result.Inputs[1].ID, result.Inputs[0].ID}) || len(result.Follows) != 1 {
		t.Fatalf("恢复后的工作 %+v", result)
	}
	f := result.Follows[0]
	if !f.Own || f.Closed != nil || f.Manual == nil || f.Manual.Own || !slices.Equal(f.Via, []string{"one"}) {
		t.Fatalf("自动跟随 %+v", f)
	}
	th := getThread(t, s, ref)
	if !slices.Equal(th.Pending, []string{"two"}) || th.Evaluating != nil || !slices.Contains(getThread(t, s, side).Subscriptions, w.ID) {
		t.Fatalf("路由后对象 %+v", th)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		d, err := r.GetDecision(engine.Key("decision", ev.ID))
		must(t, err)
		disp, err := r.GetDisposition("one")
		must(t, err)
		if d == nil || d.Kind != engine.Unavailable || d.Reason[0].Code != engine.ReasonRouted || d.Reason[0].TriggerID != "one" || !slices.Equal(disp.Fanout, []string{w.ID}) || disp.Requeued {
			t.Fatal("路由未登记")
		}
		return nil
	}))
	checkDue(t, e, 1)
	result, err = e.Attach(ctx, w.ID, "two", "")
	must(t, err)
	if len(result.Follows) != 1 || !slices.Equal(result.Follows[0].Via, []string{"one", "two"}) || len(result.Inputs) != 3 {
		t.Fatalf("同一自有对象重复挂接 %+v", result)
	}
}

func attachRefusals(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("refusals")
	tr := trigger("tr", "assign", ref)
	tr.Human = true
	tr.Addressed = "direct"
	receive(t, e, tr, engine.Stored)
	w := engine.Work{ID: "w", Mandate: engine.Mandate{Strength: engine.Explicit, Basis: engine.MandateBasis{Source: "feed", Event: "assign"}}}
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(w) }))
	_, err := e.Attach(ctx, w.ID, "missing", "")
	checkCode(t, err, engine.CodeUnknownTrigger)
	_, err = e.Attach(ctx, w.ID, "tr", "")
	checkCode(t, err, engine.CodeEvalFinished)
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	_, err = e.Attach(ctx, "absent", "tr", "")
	checkCode(t, err, engine.CodeUnknownWork)
	for _, tc := range []struct{ name, code string }{{"stopped", engine.CodeWorkStopped}, {"revoked", engine.CodeMandateRevoked}, {"revoked-ended", engine.CodeMandateRevoked}, {"worker", engine.CodeWorkerDecides}, {"returned", engine.CodeHandedBackHere}, {"conflict", engine.CodeThreadAlreadyOwned}} {
		current := w
		d := engine.TriggerDisposition{At: now, EvalID: ev.ID}
		must(t, s.Atomic(ctx, func(u engine.Unit) error {
			switch tc.name {
			case "stopped":
				current.Ended = &engine.WorkEnd{By: "human"}
			case "revoked", "revoked-ended":
				current.Mandate.Basis.Event = "missing"
				if tc.name == "revoked-ended" {
					current.Ended = &engine.WorkEnd{By: "executor"}
				}
			case "worker":
				d.Fanout = []string{w.ID}
			case "returned":
				d.Handbacks = []engine.Handback{{WorkID: w.ID}}
			case "conflict":
				th, err := u.GetThread(engine.ThreadKey(ref))
				must(t, err)
				th.Subscriptions = []string{"other"}
				must(t, u.PutThread(*th))
				must(t, u.PutWork(engine.Work{ID: "other", Follows: []engine.Follow{{Thread: th.Key, Own: true}}}))
			}
			must(t, u.PutDisposition("tr", d))
			return u.PutWork(current)
		}))
		_, err = e.Attach(ctx, w.ID, "tr", "")
		checkCode(t, err, tc.code)
		if held(t, s, ref).ID != ev.ID {
			t.Fatal("被拒的挂接消耗了研判")
		}
	}
}

func attachRecoveryGate(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	for _, tc := range []struct {
		id         string
		human, off bool
	}{{"automatic", false, false}, {"disabled", true, true}} {
		ref := item(tc.id)
		tr := trigger(tc.id, "signal", ref)
		tr.Human = tc.human
		receive(t, e, tr, engine.Stored)
		checkDue(t, e, 1)
		w := engine.Work{ID: "ended-" + tc.id, Ended: &engine.WorkEnd{By: "executor"}, Mandate: engine.Mandate{Strength: engine.Explicit, Basis: engine.MandateBasis{Source: "feed", Event: "assign"}}}
		must(t, s.Atomic(t.Context(), func(u engine.Unit) error { return u.PutWork(w) }))
		if tc.off {
			p = evalPolicy(t, engine.Settings{Events: map[string]engine.EventOverride{"feed:signal": {Mode: engine.ModeOff}}})
		}
		r, err := e.Attach(t.Context(), w.ID, tr.ID, "")
		must(t, err)
		if r.Ended == nil || len(r.Inputs) != 1 || r.Inputs[0].Kind != "trigger" {
			t.Fatalf("未满足恢复条件时应只登记输入 %+v", r)
		}
	}
}
