package enginetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func policyTransition(t *testing.T, s engine.Store) {
	ctx := t.Context()
	actor := engine.ActorRef{Source: "feed", ID: "person", Name: "请求人"}
	before := engine.Settings{Rules: []engine.Rule{{ID: "first", Enabled: true}, {ID: "second", Enabled: true}}, AllowedSenders: []engine.ActorRef{actor}}
	after := engine.Settings{Rules: []engine.Rule{{ID: "second", Enabled: true, Instruction: "新说明"}}, Events: map[string]engine.EventOverride{"feed:signal": {Mode: engine.ModeObserve}}}
	old, next := evalPolicy(t, before), evalPolicy(t, after)
	e := evalEngine(t, s, &old)
	ref := item("pending")
	receive(t, e, trigger("one", "signal", ref), engine.Stored)
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	receive(t, e, trigger("two", "signal", ref), engine.Stored)
	for _, tc := range []struct {
		id, rule, event, sender string
		strength                engine.Strength
		ended                   bool
	}{
		{"disabled", "first", "assign", "", engine.Explicit, false}, {"changed", "second", "assign", "", engine.Explicit, false}, {"sender", "", "assign", actor.Name, engine.Explicit, false},
		{"mode", "", "signal", "", engine.Inferred, false}, {"unaffected", "", "assign", "", engine.Explicit, false}, {"ended", "first", "assign", "", engine.Explicit, true},
	} {
		w := engine.Work{ID: tc.id, Mandate: engine.Mandate{Strength: tc.strength, Actor: actor, Basis: engine.MandateBasis{Via: engine.ViaEvent, Source: "feed", Event: tc.event, Sender: tc.sender}}}
		if tc.rule != "" {
			w.Mandate.Basis.Rules = []string{tc.rule}
		}
		if tc.ended {
			w.Ended = &engine.WorkEnd{}
		}
		must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(w) }))
	}
	settingsErr := errors.New("设置写入故障")
	var wakes []string
	q := &queueStore{Store: s, check: func(kind, key string, _ json.RawMessage) error {
		if kind == "delivery" {
			wakes = append(wakes, key)
		}
		return nil
	}}
	apply := func(p engine.Policy, saveErr error) error {
		return q.Atomic(ctx, func(u engine.Unit) error {
			if err := e.PolicyChanged(ctx, old, p, u); err != nil {
				return err
			}
			return saveErr
		})
	}
	must(t, apply(old, nil))
	if held(t, s, ref).ID != ev.ID || len(wakes) != 0 {
		t.Fatal("同版策略触发重排")
	}
	if err := apply(next, settingsErr); !errors.Is(err, settingsErr) {
		t.Fatal(err)
	}
	if held(t, s, ref).ID != ev.ID || len(readWork(t, s, "changed").Inputs) != 0 {
		t.Fatal("设置写入失败留下局部改版")
	}
	wakes = nil
	must(t, apply(next, nil))
	slices.Sort(wakes)
	if !slices.Equal(wakes, []string{"changed", "disabled", "mode", "sender"}) {
		t.Fatalf("唤醒 %v", wakes)
	}
	for _, id := range []string{"changed", "disabled", "mode", "sender", "unaffected", "ended"} {
		w := readWork(t, s, id)
		if id == "ended" || id == "unaffected" {
			if len(w.Inputs) != 0 {
				t.Fatal(id)
			}
			continue
		}
		if len(w.Inputs) != 1 || !slices.Equal(w.Undelivered, []string{w.Inputs[0].ID}) {
			t.Fatal(w)
		}
		in := w.Inputs[0]
		if in.Kind != "policy-changed" || in.From != old.Version() || in.To != next.Version() || in.Effect == nil {
			t.Fatalf("策略输入 %+v", in)
		}
		switch id {
		case "changed", "disabled":
			if len(in.Effect.Rules) != 1 || in.Effect.Rules[0].Change != id || (id == "changed" && (in.Effect.Rules[0].Instruction == nil || *in.Effect.Rules[0].Instruction != "新说明")) {
				t.Fatal(in.Effect)
			}
		case "mode":
			if in.Effect.Proactive == nil || *in.Effect.Proactive {
				t.Fatal(in.Effect)
			}
		case "sender":
			if in.Effect.RequesterAllowed == nil || *in.Effect.RequesterAllowed || !reflect.DeepEqual(in.Effect.Requester, &actor) {
				t.Fatal(in.Effect)
			}
		}
	}
	th := getThread(t, s, ref)
	if th.Evaluating != nil || !slices.Equal(th.Pending, []string{"one", "two"}) {
		t.Fatalf("重排研判 %+v", th)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		d, err := r.GetDecision(engine.Key("decision", ev.ID))
		must(t, err)
		if d == nil || d.Kind != engine.Unavailable || d.Reason[0].Code != engine.ReasonPolicyChanged {
			t.Fatalf("重排决策 %+v", d)
		}
		return nil
	}))
}
