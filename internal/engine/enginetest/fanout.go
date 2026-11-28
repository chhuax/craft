package enginetest

import (
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func receiveReached(t *testing.T, s engine.Store) {
	store := &intakeStore{Store: s}
	cfg := receiveConfig(t, store)
	active := cfg.Policy()
	revoked, err := engine.NewPolicy(engine.Settings{Events: map[string]engine.EventOverride{"feed:grant": {Mode: engine.ModeOff}}}, []engine.EventDef{
		{Source: "feed", Event: "grant", Default: engine.ModeAuto}, {Source: "feed", Event: "change", Default: engine.ModeAuto},
	})
	must(t, err)
	policy := active
	calls := 0
	cfg.Policy = func() engine.Policy {
		calls++
		if !store.writing {
			t.Error("在事务外取得策略")
		}
		return policy
	}
	e := newEngine(t, cfg)
	for _, tc := range []struct {
		id, endedBy                 string
		human, contextOnly, revoked bool
		wantInput, wantResume       bool
	}{
		{"open", "", false, false, true, true, false},
		{"observer", "", true, true, false, true, false},
		{"resume", "executor", true, false, false, true, true},
		{"automatic", "executor", false, false, false, false, false},
		{"stopped", "human", true, false, false, false, false},
		{"context", "executor", true, true, false, false, false},
		{"revoked", "executor", true, false, true, false, false},
	} {
		t.Run(tc.id, func(t *testing.T) {
			ref := engine.NewThreadRef("feed", "item", tc.id)
			th := engine.Thread{Ref: ref, Key: engine.ThreadKey(ref), Subscriptions: []string{"absent", tc.id}}
			w := engine.Work{ID: tc.id, Mandate: engine.Mandate{Strength: engine.Explicit, PolicyVersion: "original", Basis: engine.MandateBasis{Source: "feed", Event: "grant"}}}
			if tc.contextOnly {
				th.Context = []string{w.ID}
			}
			if tc.endedBy != "" {
				w.Ended = &engine.WorkEnd{By: tc.endedBy, Outcome: engine.Done}
				w.Follows = []engine.Follow{{Thread: th.Key, Own: true, Closed: &engine.ThreadClose{At: "2025-01-01T00:00:00.000Z", Outcome: "done"}}}
				th.ReopenedAt = now
			}
			policy = active
			if tc.revoked {
				policy = revoked
			}
			must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
				must(t, u.PutThread(th))
				return u.PutWork(w)
			}))
			before := len(store.enqueued)
			beforeCalls := calls
			tr := trigger(tc.id, "change", ref)
			tr.Human = tc.human
			receive(t, e, tr, engine.Stored)
			if calls != beforeCalls+1 {
				t.Fatalf("单条命令取策略 %d 次", calls-beforeCalls)
			}
			must(t, s.Read(t.Context(), func(r engine.Reader) error {
				got, err := r.GetWork(tc.id)
				must(t, err)
				d, err := r.GetDisposition(tr.ID)
				must(t, err)
				row, err := r.GetThread(th.Key)
				must(t, err)
				if d == nil || got == nil || row == nil || !reflect.DeepEqual(got.Mandate, w.Mandate) || (got.Ended == nil) != (w.Ended == nil || tc.wantResume) {
					t.Fatalf("恢复状态或授权异常 %+v，处置 %+v", got, d)
				}
				if !tc.wantInput {
					if len(d.Fanout) != 0 || len(got.Inputs) != 0 || len(got.Undelivered) != 0 || len(store.enqueued) != before || !slices.Equal(row.Pending, []string{tr.ID}) {
						t.Fatalf("跳过后仍认领或投递 %+v %+v %+v", got, row, d)
					}
					return nil
				}
				inputID := engine.Key("input", tc.id, "trigger", tr.ID)
				expected := engine.WorkInput{ID: inputID, Kind: "trigger", At: now, TriggerID: tr.ID, Human: tc.human, Context: tc.contextOnly}
				if len(got.Inputs) == 0 || !reflect.DeepEqual(got.Inputs[0], expected) || got.UpdatedAt != now || !slices.Equal(d.Fanout, []string{tc.id}) ||
					slices.Contains(d.Context, tc.id) != tc.contextOnly || slices.Contains(row.Pending, tr.ID) != tc.contextOnly ||
					!slices.Equal(store.enqueued[before:], []string{tc.id}) {
					t.Fatalf("输入、路由或投递异常 %+v %+v %+v %v", got, row, d, store.enqueued[before:])
				}
				waiting := []string{inputID}
				if tc.wantResume {
					if len(got.Inputs) != 2 || got.Inputs[1].Kind != "resumed" || got.Inputs[1].TriggerID != tr.ID || got.Inputs[1].At != now || got.Follows[0].Closed != nil {
						t.Fatalf("恢复输入或跟随状态 %+v", got)
					}
					waiting = []string{got.Inputs[1].ID, inputID}
				}
				if !slices.Equal(got.Undelivered, waiting) {
					t.Errorf("待投递顺序 %v，期望 %v", got.Undelivered, waiting)
				}
				return nil
			}))
		})
	}
}

func receiveRevokedFollowup(t *testing.T, s engine.Store) {
	ref := engine.NewThreadRef("feed", "item", "a")
	key := engine.ThreadKey(ref)
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutThread(engine.Thread{Key: key, Ref: ref, Subscriptions: []string{"ended"}}))
		return u.PutWork(engine.Work{ID: "ended", Ended: &engine.WorkEnd{By: "executor"},
			Mandate: engine.Mandate{Strength: engine.Explicit, Basis: engine.MandateBasis{Source: "feed", Event: "removed"}}})
	}))
	store := &intakeStore{Store: s}
	e := newEngine(t, receiveConfig(t, store))
	for _, tc := range []struct {
		event  string
		result engine.ReceiveResult
	}{{"update", engine.Filtered}, {"notice", engine.Recorded}} {
		tr := trigger(tc.event, tc.event, ref)
		tr.Human = true
		receive(t, e, tr, tc.result)
		v := readIntake(t, s, tr.ID, []string{key}, []string{"ended"})
		d := v.disposition
		w := v.works["ended"]
		if d == nil || len(d.Fanout) != 0 || len(v.threads[key].Pending) != 0 || w == nil || w.Ended == nil || len(w.Inputs) != 0 || len(store.enqueued) != 0 {
			t.Fatalf("授权失效后仍投递或待研判 %+v %+v", v, w)
		}
		if tc.result == engine.Filtered && (d.Filtered == nil || d.Filtered.Code != engine.CodeMandateRevoked) {
			t.Errorf("未订阅与撤销授权应可区分，过滤原因 %+v", d.Filtered)
		}
		if d.Recorded != (tc.result == engine.Recorded) {
			t.Errorf("事件声明的未匹配处置未生效 %+v", d)
		}
	}
}
