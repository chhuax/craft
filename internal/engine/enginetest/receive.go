package enginetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

type scope struct{}

func (scope) Scope(_ string, _ string, meta map[string]json.RawMessage) engine.ScopeResult {
	switch string(meta["skip"]) {
	case `"unknownSource"`:
		return engine.ScopeResult{Reason: engine.ReasonUnknownSource}
	case `""`:
		return engine.ScopeResult{}
	}
	return engine.ScopeResult{Listened: true}
}

func (scope) Closable(string, string) bool { return false }

func receiveConfig(t *testing.T, store engine.Store) engine.Config {
	t.Helper()
	p, err := engine.NewPolicy(engine.Settings{}, []engine.EventDef{
		{Source: "feed", Event: "change", Default: engine.ModeAuto},
		{Source: "feed", Event: "grant", Default: engine.ModeAuto},
		{Source: "feed", Event: "update", FollowupOnly: true},
		{Source: "feed", Event: "notice", FollowupOnly: true, OnUnmatched: "record"},
	})
	must(t, err)
	cfg := config(store)
	cfg.Scope = scope{}
	cfg.Policy = func() engine.Policy { return p }
	return cfg
}

func trigger(id, event string, refs ...engine.ThreadRef) engine.Trigger {
	return engine.Trigger{ID: id, Event: event, Source: "feed", At: "2026-01-01T08:00:00+08:00", Threads: refs}
}

func receive(t *testing.T, e *engine.Engine, tr engine.Trigger, want engine.ReceiveResult) {
	t.Helper()
	got, err := e.Receive(t.Context(), tr)
	must(t, err)
	if got != want {
		t.Fatalf("事件 %q 处置 %q，期望 %q", tr.ID, got, want)
	}
}

func receiveUnclaimed(t *testing.T, s engine.Store) {
	ctx := t.Context()
	store := &intakeStore{Store: s}
	e := newEngine(t, receiveConfig(t, store))
	a := engine.NewThreadRef("feed", "item", "a")
	b := engine.NewThreadRef("feed", "item", "b")
	keys := []string{engine.ThreadKey(a), engine.ThreadKey(b)}
	tr := trigger("first", "change", a, b, a)
	tr.Text = strings.Repeat("字", 4001)
	tr.Meta = map[string]json.RawMessage{"parent": json.RawMessage(`"previous"`)}
	receive(t, e, tr, engine.Stored)
	for _, tc := range []struct {
		id, event, scopeReason string
		result                 engine.ReceiveResult
		reason                 string
	}{
		{"late", "change", "", engine.Stored, ""},
		{"unmatched", "update", "", engine.Filtered, engine.ReasonFollowupOnly},
		{"notice", "notice", "", engine.Recorded, ""},
		{"outside", "notice", "unknownSource", engine.Filtered, engine.ReasonUnknownSource},
		{"off", "change", "-", engine.Filtered, engine.ReasonSourceOff},
	} {
		ref := a
		if tc.scopeReason != "" {
			ref = engine.NewThreadRef("feed", "item", tc.id)
		}
		input := trigger(tc.id, tc.event, ref)
		input.At = "2025-12-31T12:34:56Z"
		if tc.scopeReason != "" {
			reason := tc.scopeReason
			if reason == "-" {
				reason = ""
			}
			raw, err := json.Marshal(reason)
			must(t, err)
			input.Meta = map[string]json.RawMessage{"skip": raw}
		}
		beforeEvents := len(store.staged)
		receive(t, e, input, tc.result)
		must(t, s.Read(ctx, func(r engine.Reader) error {
			got, err := r.GetTrigger(tc.id)
			must(t, err)
			d, err := r.GetDisposition(tc.id)
			must(t, err)
			if got == nil || got.At != "2025-12-31T12:34:56.000Z" || d == nil || d.At != now || d.Recorded != (tc.result == engine.Recorded) || len(d.Fanout) != 0 {
				t.Fatalf("%s 未正确记录事件与处置 %+v %+v", tc.id, got, d)
			}
			if (d.Filtered != nil) != (tc.reason != "") || (d.Filtered != nil && d.Filtered.Code != tc.reason) {
				t.Errorf("%s 过滤原因 %+v", tc.id, d.Filtered)
			}
			if len(store.staged) != beforeEvents+1 || store.staged[beforeEvents] != "trigger.received" {
				t.Errorf("处置 %s 后缺少接收通知", tc.result)
			}
			if tc.scopeReason != "" {
				th, err := r.GetThread(engine.ThreadKey(ref))
				must(t, err)
				links, err := r.ThreadKeys(tc.id)
				must(t, err)
				if th != nil || len(links) != 0 {
					t.Errorf("范围外仍建立对象或关联 %v %v", th, links)
				}
			}
			return nil
		}))
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		ths, err := r.GetThreads(keys...)
		must(t, err)
		for i, key := range keys {
			th := ths[key]
			if th == nil || th.LastAt != "2026-01-01T00:00:00.000Z" || !slices.Equal(th.Correlated, []string{keys[1-i]}) {
				t.Fatalf("对象最后时间或相关对象错误 %+v", th)
			}
		}
		if !slices.Equal(ths[keys[0]].Pending, []string{"first", "late"}) || len(ths[keys[1]].Pending) != 0 {
			t.Fatalf("主、次对象的 pending %v %v", ths[keys[0]].Pending, ths[keys[1]].Pending)
		}
		links, err := r.ThreadKeys(tr.ID)
		must(t, err)
		slices.Sort(links)
		ids, err := r.TriggerIDs(keys[1])
		must(t, err)
		if !slices.Equal(links, keys) || !slices.Equal(ids, []string{tr.ID}) {
			t.Errorf("双向关联错误 %v %v", links, ids)
		}
		stored, err := r.GetTrigger(tr.ID)
		must(t, err)
		if stored == nil || stored.Text != tr.Text || !reflect.DeepEqual(stored.Meta, tr.Meta) || !slices.Equal(stored.Threads, tr.Threads) {
			t.Errorf("事件快照被改写 %+v", stored)
		}
		return nil
	}))
	if len(store.enqueued) != 0 || len(store.workReads) != 0 {
		t.Fatalf("无订阅仍查询工作或登记投递 %v %v", store.workReads, store.enqueued)
	}
}

func receiveInvalid(t *testing.T, s engine.Store) {
	e := newEngine(t, receiveConfig(t, s))
	withoutRefs := trigger("x", "change")
	invalidTime := trigger("x", "change", engine.NewThreadRef("feed", "item", "a"))
	invalidTime.At = "?"
	for _, tr := range []engine.Trigger{withoutRefs, invalidTime} {
		_, err := e.Receive(t.Context(), tr)
		var rejection *engine.UserError
		if err == nil || errors.As(err, &rejection) {
			t.Fatalf("非法事件应返回编程错误，得到 %v", err)
		}
	}
}
