package enginetest

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func checkScratchTarget(t *testing.T, w engine.Work) {
	t.Helper()
	want := &engine.TargetSnapshot{ID: "scratch", Kind: "scratch", Title: w.Title, Evidence: []string{"v1 无选址，默认在临时目录执行"}, SelectedBy: "default"}
	if w.Execution == nil || !reflect.DeepEqual(w.Execution.Target, want) {
		t.Fatalf("Work %s 的默认目标错误：%+v", w.ID, w.Execution)
	}
}

func decideAccept(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	var launches []string
	fail := true
	store := &queueStore{Store: s, check: func(kind, key string, value json.RawMessage) error {
		if kind == "launch" {
			var payload struct{ WorkID, AssignmentID string }
			must(t, json.Unmarshal(value, &payload))
			if key != payload.AssignmentID || payload.WorkID == "" {
				t.Fatalf("派工请求 %s", value)
			}
			launches = append(launches, key)
			if fail {
				return errQueue
			}
		}
		return nil
	}}
	e := evalEngine(t, store, &p)
	a, b := item("origin"), item("companion")
	channel := &engine.ChannelRef{Source: "feed", Thread: json.RawMessage(`"room"`)}
	for _, id := range []string{"one", "two"} {
		tr := trigger(id, "assign", a, b)
		tr.Actor.ID = id
		tr.Channel = channel
		receive(t, e, tr, engine.Stored)
	}
	checkDue(t, e, 1)
	ev := held(t, s, a)
	wantID := engine.Key("work", ev.ID)
	out := engine.Outcome{Kind: engine.Accept, TriggerID: "one", EvalID: ev.ID, Scope: "核对交付"}
	keys := []string{engine.ThreadKey(a), engine.ThreadKey(b)}
	before := readIntake(t, s, "one", keys, []string{wantID})
	_, err := e.Decide(t.Context(), out)
	if !errors.Is(err, errQueue) {
		t.Fatalf("预期入队失败，返回 %v", err)
	}
	if after := readIntake(t, s, "one", keys, []string{wantID}); !reflect.DeepEqual(before, after) {
		t.Fatalf("入队失败后残留状态 %+v", after)
	}
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		ds, err := r.DecisionsByThread(keys[0])
		must(t, err)
		if len(ds) != 0 {
			t.Fatal("决策未回滚")
		}
		return nil
	}))
	fail = false
	d, err := e.Decide(t.Context(), out)
	must(t, err)
	state := readIntake(t, s, "two", keys, []string{wantID})
	w := state.works[wantID]
	if d.Kind != engine.Accept || w == nil || d.WorkID != wantID || d.ID != w.DecisionID || w.Mandate.Scope != out.Scope || w.Mandate.PolicyVersion != p.Version() || w.Mandate.Actor.ID != ev.Primary ||
		d.Mandate == nil || !reflect.DeepEqual(*d.Mandate, w.Mandate) || len(w.Channels) != 1 || !slices.Equal(w.Origin.TriggerIDs, ev.TriggerIDs) || w.Origin.TriggerID != ev.Primary {
		t.Fatalf("授权或起源错误 %+v %+v", w, d)
	}
	if len(w.Assignments) != 1 || w.Assignments[0].State != engine.AssignmentPending || !slices.Equal(launches, []string{w.Assignments[0].ID, w.Assignments[0].ID}) ||
		len(w.Follows) != 1 || !w.Follows[0].Own || w.Follows[0].Thread != keys[0] || !slices.Equal(w.Follows[0].Via, ev.TriggerIDs) {
		t.Fatalf("派工或自有对象 %+v", w)
	}
	for _, key := range keys {
		if !slices.Equal(state.threads[key].Subscriptions, []string{wantID}) || state.threads[key].Evaluating != nil {
			t.Fatalf("对象 %s %+v", key, state.threads[key])
		}
	}
	if state.disposition.DecisionID != d.ID {
		t.Fatal("批内其他事件缺少决策")
	}
	checkScratchTarget(t, *w)
	for _, eid := range []string{ev.ID, ""} {
		out.EvalID = eid
		replay, err := e.Decide(t.Context(), out)
		must(t, err)
		if replay.ID != d.ID || len(launches) != 2 {
			t.Fatal("重复接单产生了新决策或派工")
		}
	}
}

func presetTarget(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("selected")
	receive(t, e, trigger("select", "assign", ref), engine.Stored)
	checkDue(t, e, 1)
	target := &engine.TargetSnapshot{ID: "chosen", Kind: "dir", Path: filepath.Join(t.TempDir(), "project"), Title: "已选目录", Evidence: []string{"研判已指定"}, SelectedBy: "assessor"}
	out := engine.Outcome{Kind: engine.Accept, EvalID: held(t, s, ref).ID, TriggerID: "select", Target: target}
	d, err := e.Decide(ctx, out)
	must(t, err)
	out.Target = nil
	replay, err := e.Decide(ctx, out)
	must(t, err)
	w := readWork(t, s, d.WorkID)
	if replay.ID != d.ID || len(w.Assignments) != 1 || w.Execution == nil || !reflect.DeepEqual(w.Execution.Target, target) {
		t.Fatalf("预设目标未保留到重放后：%+v", w.Execution)
	}
}

func decisionModes(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ctx := t.Context()
	_, err := e.Decide(ctx, engine.Outcome{TriggerID: "missing", EvalID: "old", Kind: engine.Accept})
	checkCode(t, err, engine.CodeUnknownTrigger)
	for _, tc := range []struct {
		id, event  string
		kind, want engine.DecisionKind
		reason     string
	}{
		{"observe", "watch", engine.Accept, engine.Ignore, engine.ReasonObserved},
		{"refuse", "signal", engine.Accept, "", engine.CodeNotExplicit},
		{"ignored", "assign", engine.Ignore, engine.Ignore, engine.ReasonSaid},
		{"failure", "assign", engine.Unavailable, engine.Unavailable, engine.ReasonTimeout},
		{"version", "watch", engine.Accept, engine.Unavailable, engine.ReasonPolicyChanged},
	} {
		ref := item(tc.id)
		receive(t, e, trigger(tc.id, tc.event, ref), engine.Stored)
		checkDue(t, e, 1)
		ev := held(t, s, ref)
		out := engine.Outcome{TriggerID: tc.id, EvalID: "wrong", Kind: tc.kind, Reason: engine.Reason{Code: engine.ReasonSaid, Values: map[string]string{"text": "依据"}}, Scope: "核对", Audit: map[string]json.RawMessage{"evidence": json.RawMessage(`"事实"`)}}
		_, err := e.Decide(ctx, out)
		checkCode(t, err, engine.CodeEvalFinished)
		out.EvalID = ev.ID
		switch tc.id {
		case "version":
			p = evalPolicy(t, engine.Settings{Rules: []engine.Rule{{ID: "changed"}}})
		case "failure":
			out.Reason.Code = engine.ReasonTimeout
		}
		d, err := e.Decide(ctx, out)
		if tc.want == "" {
			checkCode(t, err, tc.reason)
			if held(t, s, ref).ID != ev.ID {
				t.Fatal("拒绝时研判已结束")
			}
			continue
		}
		must(t, err)
		if d.Kind != tc.want || len(d.Reason) == 0 || d.Reason[0].Code != tc.reason || d.Dismissed {
			t.Fatalf("结论 %+v", d)
		}
		if tc.id == "observe" {
			var prop struct {
				Title, Scope, Target string
				Rules                []string
			}
			must(t, json.Unmarshal(d.Audit["proposal"], &prop))
			if string(d.Audit["would_accept"]) != "true" || string(d.Audit["evidence"]) != `"事实"` || prop.Title == "" || prop.Scope != out.Scope || prop.Target != ev.ThreadKey {
				t.Fatalf("观察结论 %+v", d)
			}
		}
		if tc.id == "version" && !slices.Equal(getThread(t, s, ref).Pending, ev.TriggerIDs) {
			t.Fatal("策略改版未重排")
		}
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		works, err := r.OpenWorks()
		must(t, err)
		if len(works) != 0 {
			t.Fatal("未授权却产生工作")
		}
		return nil
	}))
}

func workerDecides(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("live")
	key := engine.ThreadKey(ref)
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutWork(engine.Work{ID: "w"}))
		return u.PutThread(engine.Thread{Key: key, Ref: ref, Subscriptions: []string{"w"}})
	}))
	for _, tc := range []struct {
		id, event, target string
		want              engine.DecisionKind
		reason            string
	}{
		{"normal", "assign", "w", engine.Stop, engine.ReasonHumanStop}, {"unknown", "assign", "absent", engine.Ignore, engine.ReasonStopUnknown},
		{"weak", "signal", "w", engine.Ignore, engine.ReasonStopNotExplicit},
	} {
		tr := trigger(tc.id, tc.event, ref)
		tr.Human = true
		tr.Addressed = "direct"
		receive(t, e, tr, engine.Stored)
		_, err := e.Decide(t.Context(), engine.Outcome{TriggerID: tr.ID, Kind: engine.Ignore})
		checkCode(t, err, engine.CodeWorkerDecides)
		d, err := e.Decide(t.Context(), engine.Outcome{TriggerID: tr.ID, Kind: engine.Stop, WorkID: tc.target, Reason: engine.Reason{Code: engine.ReasonHumanStop}})
		must(t, err)
		if d.Kind != tc.want || len(d.Reason) == 0 || d.Reason[0].Code != tc.reason {
			t.Fatalf("停止研判 %+v", d)
		}
	}
}

func ownExclusion(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	for _, tc := range []struct {
		id                         string
		same, ended, own, conflict bool
	}{
		{"conflict", true, false, true, true}, {"elsewhere", false, false, true, false}, {"finished", true, true, true, false}, {"shared", true, false, false, false},
	} {
		ref := item(tc.id)
		receive(t, e, trigger(tc.id, "assign", ref), engine.Stored)
		checkDue(t, e, 1)
		ev := held(t, s, ref)
		th := getThread(t, s, ref)
		other := item("other-" + tc.id)
		if tc.same {
			other = ref
		}
		w := engine.Work{ID: "previous-" + tc.id, Follows: []engine.Follow{{Thread: engine.ThreadKey(other), Own: tc.own}}}
		if tc.ended {
			w.Ended = &engine.WorkEnd{By: "executor"}
		}
		th.Subscriptions = []string{w.ID}
		must(t, s.Atomic(t.Context(), func(u engine.Unit) error { must(t, u.PutWork(w)); return u.PutThread(th) }))
		_, err := e.Decide(t.Context(), engine.Outcome{TriggerID: tc.id, EvalID: ev.ID, Kind: engine.Accept})
		if tc.conflict {
			checkCode(t, err, engine.CodeThreadAlreadyOwned)
			if held(t, s, ref).ID != ev.ID {
				t.Fatal("冲突消耗了研判")
			}
		} else {
			must(t, err)
		}
	}
}

// 外部写事务已处理过批内一条事件。只让未处理的同批事件重排，不可再授权。
func acceptedDuringEval(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	for _, test := range []struct {
		id     string
		extra  bool
		reason string
	}{{"single", false, engine.ReasonStartedWork}, {"mixed", true, engine.ReasonAcceptedBefore}} {
		ref := item(test.id)
		receive(t, e, trigger(test.id, "assign", ref), engine.Stored)
		if test.extra {
			receive(t, e, trigger("sibling", "signal", ref), engine.Stored)
		}
		checkDue(t, e, 1)
		ev := held(t, s, ref)
		old := engine.Decision{ID: "prior-" + test.id, EvalID: "original", ThreadKey: ev.ThreadKey, Kind: engine.Accept, WorkID: "previous", TriggerIDs: []string{test.id}}
		must(t, s.Atomic(t.Context(), func(u engine.Unit) error { return u.PutDecision(old) }))
		d, err := e.Decide(t.Context(), engine.Outcome{Kind: engine.Accept, EvalID: ev.ID, TriggerID: test.id})
		must(t, err)
		th := getThread(t, s, ref)
		if d.ID != old.ID || th.Evaluating != nil || !slices.Equal(th.Pending, ev.TriggerIDs[1:]) {
			t.Fatalf("重复接单后 %v %+v", d, th)
		}
		must(t, s.Read(t.Context(), func(r engine.Reader) error {
			record, err := r.GetDecision(engine.Key("decision", ev.ID))
			must(t, err)
			if record == nil || len(record.Reason) != 1 || record.Reason[0].Code != test.reason {
				t.Fatalf("批次收尾 %+v", record)
			}
			return nil
		}))
	}
}

func ownershipOnResume(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref, second := item("origin"), item("other")
	key := engine.ThreadKey(ref)
	old := engine.Work{ID: "previous", Ended: &engine.WorkEnd{By: "executor"}, Mandate: engine.Mandate{Strength: engine.Explicit, Basis: engine.MandateBasis{Source: "feed", Event: "assign"}}, Follows: []engine.Follow{{Thread: key, Own: true}}}
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutWork(old))
		must(t, u.PutThread(engine.Thread{Key: key, Ref: ref, Subscriptions: []string{old.ID}}))
		return u.PutThread(engine.Thread{Key: engine.ThreadKey(second), Ref: second, Subscriptions: []string{old.ID}})
	}))
	receive(t, e, trigger("automatic", "assign", ref), engine.Stored)
	checkDue(t, e, 1)
	d, err := e.Decide(t.Context(), engine.Outcome{Kind: engine.Accept, TriggerID: "automatic", EvalID: held(t, s, ref).ID})
	must(t, err)
	// 在别的对象上收到人消息也不能让历史工作重新占用已转交的自有对象。
	tr := trigger("message", "assign", second)
	tr.Human = true
	tr.Addressed = "direct"
	receive(t, e, tr, engine.Stored)
	checkDue(t, e, 1)
	_, err = e.Attach(t.Context(), old.ID, tr.ID, "")
	checkCode(t, err, engine.CodeThreadAlreadyOwned)
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		prev, err := r.GetWork(old.ID)
		must(t, err)
		current, err := r.GetWork(d.WorkID)
		must(t, err)
		if prev.Ended == nil || len(prev.Inputs) != 0 || current.Ended != nil {
			t.Fatal("恢复时破坏自有对象互斥")
		}
		return nil
	}))
}

func ownCapabilities(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	for _, tc := range []struct {
		id, kind string
		own      bool
	}{{"close", "item", true}, {"persistent", "stream", false}} {
		ref := engine.NewThreadRef("feed", tc.kind, tc.id)
		receive(t, e, trigger(tc.id, "assign", ref), engine.Stored)
		checkDue(t, e, 1)
		ev := held(t, s, ref)
		must(t, e.ThreadClosed(t.Context(), ref, "done", now, ""))
		d, err := e.Decide(t.Context(), engine.Outcome{Kind: engine.Accept, TriggerID: tc.id, EvalID: ev.ID})
		must(t, err)
		must(t, s.Read(t.Context(), func(r engine.Reader) error {
			w, err := r.GetWork(d.WorkID)
			must(t, err)
			if (len(w.Follows) != 0) != tc.own || (tc.own && (!w.Follows[0].Own || w.Follows[0].Closed == nil || w.Follows[0].Closed.At != now)) {
				t.Fatalf("对象能力或关闭事实 %+v", w.Follows)
			}
			return nil
		}))
	}
}

func admissionRules(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{Rules: []engine.Rule{{ID: "allow", Enabled: true}, {ID: "disabled"}}})
	e := evalEngine(t, s, &p)
	ref := item("rules")
	receive(t, e, trigger("candidate", "signal", ref), engine.Stored)
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	out := engine.Outcome{Kind: engine.Accept, TriggerID: ev.Primary, EvalID: ev.ID}
	for _, rules := range [][]string{nil, {"absent"}, {"allow", "disabled"}, {"allow"}} {
		out.Rules = rules
		d, err := e.Decide(t.Context(), out)
		if !slices.Equal(rules, []string{"allow"}) {
			checkCode(t, err, engine.CodeNotExplicit)
			continue
		}
		must(t, err)
		if d.Mandate == nil || d.Mandate.Strength != engine.Inferred || !slices.Equal(d.Mandate.Basis.Rules, rules) {
			t.Fatalf("命中规则未进入授权 %+v", d.Mandate)
		}
	}
}

func nextBatch(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("subsequent")
	receive(t, e, trigger("head", "assign", ref), engine.Stored)
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	for _, id := range []string{"later-1", "later-2"} {
		receive(t, e, trigger(id, "assign", ref), engine.Stored)
	}
	checkDue(t, e, 0)
	_, err := e.Decide(t.Context(), engine.Outcome{Kind: engine.Ignore, TriggerID: "head", EvalID: ev.ID})
	must(t, err)
	checkDue(t, e, 1)
	next := held(t, s, ref)
	if next.ID == ev.ID || !slices.Equal(next.TriggerIDs, []string{"later-1", "later-2"}) {
		t.Fatalf("后续批次 %+v", next)
	}
}
