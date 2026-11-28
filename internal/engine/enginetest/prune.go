package enginetest

import (
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func pruneRetention(t *testing.T, s engine.Store) {
	ctx := t.Context()
	before, err := engine.AddMs(now, -1)
	must(t, err)
	after, err := engine.AddMs(now, 1)
	must(t, err)
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		seedPruneWorks(t, u, before, after)
		seedPruneThreads(t, u, before, after)
		must(t, u.PutDisposition("old", engine.TriggerDisposition{DecisionID: "old", At: before}))
		must(t, u.LinkTrigger("old", "old"))
		must(t, u.LinkTrigger("boundary", "linked"))
		must(t, u.Effects().Enqueue("stop", "assignment", []byte(`{"workId":"effect"}`)))
		return u.Effects().Enqueue("assessment", "eval", []byte(`{"triggerId":"effectTrigger","threadKey":"effectThread"}`))
	}))
	cfg := config(s)
	// 实际排队的回复自带渠道和正文，执行时无需再按派生的消息 ID 找回原事件。
	receipt, err := newEngine(t, cfg).Reply(ctx, engine.ReplyRequest{Kind: engine.ReplyReply, TriggerID: "replySource", Text: "收到"})
	must(t, err)
	cfg.Clock = func() string { t.Fatal("清理读取了时钟"); return "" }
	e := newEngine(t, cfg)
	got, err := e.Prune(ctx, now, now, now)
	must(t, err)
	want := engine.PruneResult{Works: 1, Decisions: 1, Threads: 1, Triggers: 2, Dispositions: 1, TriggerThreads: 1, WorkIDs: []string{"old"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("清理结果 %+v，期望 %+v", got, want)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		checkPruneSurvivors(t, r)
		checkPruneIndexes(t, r)
		queued, err := r.HasEffect("reply", receipt.ID)
		must(t, err)
		if !queued {
			t.Fatal("原事件清理后回复请求丢失")
		}
		return nil
	}))
	got, err = e.Prune(ctx, now, now, now)
	must(t, err)
	if !reflect.DeepEqual(got, engine.PruneResult{}) {
		t.Fatal("重复清理", got)
	}
}

func seedPruneWorks(t *testing.T, u engine.Unit, before, after string) {
	t.Helper()
	end := &engine.WorkEnd{At: before}
	for _, w := range []engine.Work{
		{ID: "old", Ended: end, DecisionID: "old"},
		{ID: "boundary", Ended: &engine.WorkEnd{At: now}, Origin: engine.Origin{TriggerIDs: []string{"batch"}}},
		{ID: "new", Ended: &engine.WorkEnd{At: after}, Attention: &engine.Question{TriggerID: "question"}},
		{
			ID: "open", DecisionID: "grant", Origin: engine.Origin{TriggerID: "origin"},
			Inputs:  []engine.WorkInput{{Kind: "resumed", TriggerID: "input"}, {Kind: "reopen", DecisionID: "reopened"}},
			Follows: []engine.Follow{{Thread: "follow", Via: []string{"via"}}}, Answers: []string{"answer"},
		},
		{ID: "effect", Ended: end, DecisionID: "indirect"},
		{ID: "decision", Ended: end},
	} {
		must(t, u.PutWork(w))
	}
	for _, d := range []engine.Decision{
		{ID: "old", At: before, WorkID: "old", Kind: engine.Accept, TriggerIDs: []string{"old"}},
		{ID: "boundary", At: now, ThreadKey: "decision", TriggerIDs: []string{"fromDecision"}, WorkID: "decision"},
		{ID: "new", At: after}, {ID: "grant", At: before}, {ID: "reopened", At: before},
		{ID: "indirect", At: before, TriggerIDs: []string{"indirect"}},
	} {
		must(t, u.PutDecision(d))
	}
}

func seedPruneThreads(t *testing.T, u engine.Unit, before, after string) {
	t.Helper()
	ref := item("embedded")
	closed := &engine.ThreadClose{At: before}
	for _, th := range []engine.Thread{
		{Key: "old", Closed: closed, Subscriptions: []string{"old"}, Context: []string{"old"}},
		{Key: "boundary", Closed: &engine.ThreadClose{At: now}}, {Key: "new", Closed: &engine.ThreadClose{At: after}},
		{Key: "open", Subscriptions: []string{"old", "open"}, Context: []string{"old"}},
		{Key: "follow", Closed: closed}, {Key: "subscription", Closed: closed, Subscriptions: []string{"open"}},
		{Key: "decision", Closed: closed}, {Key: "pending", Closed: closed, Pending: []string{"pending"}},
		{Key: "eval", Closed: closed, Evaluating: &engine.Evaluation{ID: "eval", Primary: "primary", TriggerIDs: []string{"evaluating"}}},
		{Key: "linked", Closed: closed}, {Key: engine.ThreadKey(ref), Closed: closed}, {Key: "effectThread", Closed: closed},
	} {
		must(t, u.PutThread(th))
	}
	triggers := []string{"old", "replySource", "batch", "origin", "question", "input", "via", "answer", "fromDecision", "pending", "primary", "evaluating", "indirect", "effectTrigger", "boundary", "new"}
	for _, id := range triggers {
		tr := engine.Trigger{ID: id, At: before}
		switch id {
		case "replySource":
			tr.Human = true
			tr.Threads = []engine.ThreadRef{ref}
			tr.Channel = &engine.ChannelRef{Source: "feed", Thread: []byte(`"room"`)}
		case "boundary":
			tr.At = now
			tr.Threads = []engine.ThreadRef{ref}
		case "new":
			tr.At = after
		}
		must(t, u.PutTrigger(tr))
	}

}

func checkPruneSurvivors(t *testing.T, r engine.Reader) {
	t.Helper()
	work, err := r.GetWorks("old", "boundary", "new", "open", "effect", "decision")
	must(t, err)
	if len(work) != 5 || work["old"] != nil {
		t.Fatal("保留的 Work", work)
	}
	decisions, err := r.GetDecisions("old", "boundary", "new", "grant", "reopened", "indirect")
	must(t, err)
	if len(decisions) != 5 || decisions["old"] != nil {
		t.Fatal("保留的 Decision", decisions)
	}
	threads, err := r.GetThreads("old", "boundary", "new", "open", "follow", "subscription", "decision", "pending", "eval", "linked", engine.ThreadKey(item("embedded")), "effectThread")
	must(t, err)
	if len(threads) != 11 || threads["old"] != nil {
		t.Fatal("保留的 Thread", threads)
	}
	triggers, err := r.GetTriggers("old", "replySource", "batch", "origin", "question", "input", "via", "answer", "fromDecision", "pending", "primary", "evaluating", "indirect", "effectTrigger", "boundary", "new")
	must(t, err)
	if len(triggers) != 14 || triggers["old"] != nil || triggers["replySource"] != nil {
		t.Fatal("保留的 Trigger", triggers)
	}
}

func checkPruneIndexes(t *testing.T, r engine.Reader) {
	t.Helper()
	th, err := r.GetThread("open")
	must(t, err)
	if !slices.Equal(th.Subscriptions, []string{"open"}) || len(th.Context) != 0 {
		t.Error(th)
	}
	keys, err := r.SubscribedThreadKeys("old")
	must(t, err)
	links, err := r.TriggerIDs("old")
	must(t, err)
	accepted, err := r.AcceptedByTrigger("old")
	must(t, err)
	disp, err := r.GetDisposition("old")
	must(t, err)
	if len(keys)+len(links)+len(accepted) != 0 || disp != nil {
		t.Fatal("级联索引残留", keys, links, accepted, disp)
	}

}

func pruneEvents(t *testing.T, s engine.Store) {
	ctx := t.Context()
	before, err := engine.AddMs(now, -1)
	must(t, err)
	after, err := engine.AddMs(now, 1)
	must(t, err)
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		for _, at := range []string{before, now, before, after} {
			must(t, u.Emit(engine.Event{At: at}))
		}
		for range 5000 {
			must(t, u.Emit(engine.Event{At: before}))
		}
		return nil
	}))
	e := newEngine(t, config(s))
	got, err := e.Prune(ctx, now, now, now)
	must(t, err)
	if got.Events != 2 {
		t.Fatalf("清理事件 %d", got.Events)
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.Emit(engine.Event{At: now}) }))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		ev, err := r.EventsSince(0, 6000)
		must(t, err)
		if len(ev) != 5003 || ev[0].Rev != 2 || ev[1].Rev != 4 || ev[2].Rev != 5 || ev[len(ev)-1].Rev != 5005 {
			t.Fatal("补拉边界或计数器错误", len(ev))
		}
		return nil
	}))
}

func pruneThreadReferences(t *testing.T, s engine.Store) {
	ctx := t.Context()
	cutoff, err := engine.AddMs(now, 1)
	must(t, err)
	closeAt := &engine.ThreadClose{At: now}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		for _, th := range []engine.Thread{
			{Key: "a", Closed: closeAt, Correlated: []string{"b"}},
			{Key: "b", Closed: closeAt, Correlated: []string{"a"}},
			{Key: "open", Correlated: []string{"referenced"}},
			{Key: "referenced", Closed: closeAt},
			{Key: "first", Closed: closeAt, Correlated: []string{"last"}},
			{Key: "last", Closed: closeAt},
		} {
			must(t, u.PutThread(th))
		}
		return nil
	}))
	e := newEngine(t, config(s))
	got, err := e.Prune(ctx, now, cutoff, now)
	must(t, err)
	if got.Threads != 2 {
		t.Fatalf("仍被其他 Thread 引用的关闭对象须保留，实际删除 %d", got.Threads)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		kept, err := r.GetThreads("a", "b", "open", "referenced", "first", "last")
		must(t, err)
		if len(kept) != 4 || kept["first"] != nil || kept["last"] != nil {
			t.Fatalf("清理后的 Thread %v", kept)
		}
		return nil
	}))
	got, err = e.Prune(ctx, now, cutoff, now)
	must(t, err)
	if got.Threads != 0 {
		t.Fatal("重复清理改变了关联对象", got)
	}
}

func pruneUpdatedReferences(t *testing.T, s engine.Store) {
	ctx := t.Context()
	old, err := engine.AddMs(now, -1)
	must(t, err)
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		for _, id := range []string{"old", "current", "recentDisposition"} {
			must(t, u.PutTrigger(engine.Trigger{ID: id, At: old}))
		}
		must(t, u.PutDisposition("recentDisposition", engine.TriggerDisposition{At: now}))
		for _, id := range []string{"old", "current"} {
			must(t, u.PutWork(engine.Work{ID: "w", Origin: engine.Origin{TriggerID: id}}))
			must(t, u.PutDecision(engine.Decision{ID: "d", At: now, TriggerIDs: []string{id}}))
			must(t, u.PutThread(engine.Thread{Key: "th", Pending: []string{id}}))
			must(t, u.Effects().Enqueue("assessment", "pending", []byte(`{"triggerId":"`+id+`"}`)))
		}
		return nil
	}))
	e := newEngine(t, config(s))
	got, err := e.Prune(ctx, now, now, now)
	must(t, err)
	if got.Triggers != 1 {
		t.Fatal("已覆盖的引用仍挡住旧事件，或误删新引用", got)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		items, err := r.GetTriggers("old", "current", "recentDisposition")
		must(t, err)
		if len(items) != 2 || items["old"] != nil {
			t.Fatal(items)
		}
		return nil
	}))
}
