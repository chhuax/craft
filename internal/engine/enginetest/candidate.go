package enginetest

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func candidateCards(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("main")
	long := strings.Repeat("🌱", 4001)
	tr := trigger("origin", "assign", ref)
	tr.Text = long
	receive(t, e, tr, engine.Stored)
	checkDue(t, e, 1)
	th := getThread(t, s, ref)
	th.Subscriptions = []string{"local", "local-ended"}
	facts := map[string]engine.Fact{"title": {Value: json.RawMessage(`"主题"`)}}
	th.Meta.Facts = facts
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutThread(th))
		must(t, u.PutWork(engine.Work{ID: "local", CreatedAt: "9999", Execution: &engine.Execution{Assignment: long},
			Inputs: []engine.WorkInput{{Kind: "text", Text: &long}}, Assignments: []engine.Assignment{{Report: &engine.Report{Summary: long}}}}))
		must(t, u.PutWork(engine.Work{ID: "local-ended", Ended: &engine.WorkEnd{}}))
		for i := 10; i >= 0; i-- {
			id := fmt.Sprintf("r%02d", i)
			other := item(id)
			k := engine.ThreadKey(other)
			w := engine.Work{ID: id, Title: id, CreatedAt: fmt.Sprintf("2026-01-%02dT00:00:00.000Z", i+1), Follows: []engine.Follow{{Thread: k, Own: true}, {Thread: "context"}}}
			if i == 9 {
				w.Ended = &engine.WorkEnd{}
			}
			must(t, u.PutWork(w))
			must(t, u.PutThread(engine.Thread{Key: k, Ref: other, Meta: engine.ThreadMeta{Facts: facts}}))
		}
		return nil
	}))
	c, err := e.Candidate(t.Context(), th.Evaluating.ID)
	must(t, err)
	if c.Thread.Key != th.Key || string(c.Thread.Meta.Facts["title"].Value) != `"主题"` || len(c.Triggers) != 1 || len(c.Works) != 2 || c.Works[1].Ended == nil || len(c.Related) != 8 {
		t.Fatalf("候选卡片内容不全 %+v", c.Thread)
	}
	w := c.Works[0]
	for limit, got := range map[int]string{4000: c.Triggers[0].Text, 400: *w.Inputs[0].Text, 600: w.Assignments[0].Report.Summary, 1500: w.Execution.Assignment} {
		if got != strings.Repeat("🌱", limit) {
			t.Fatalf("字符预算 %d，返回 %d 个字符", limit, len([]rune(got)))
		}
	}
	for i, id := range []string{"r10", "r08", "r07", "r06", "r05", "r04", "r03", "r02"} {
		r := c.Related[i]
		k := engine.ThreadKey(item(id))
		if r.ID != id || r.Title != id || r.Stage.Stage != "working" || !slices.Equal(r.Threads, []string{k}) || len(r.Facts) != 1 || string(r.Facts[k]["title"].Value) != `"主题"` {
			t.Fatalf("相关卡片[%d] %+v", i, r)
		}
	}
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		full, err := r.GetWork("local")
		must(t, err)
		if full.Execution.Assignment != long || *full.Inputs[0].Text != long {
			t.Fatal("卡片截断污染仓储")
		}
		return nil
	}))
	_, err = e.Candidate(t.Context(), "past")
	checkCode(t, err, engine.CodeEvalFinished)
}

func rulesAssessment(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{Rules: []engine.Rule{{ID: "enabled", Enabled: true}}})
	e := evalEngine(t, s, &p)
	for _, tc := range []struct {
		event string
		want  engine.DecisionKind
	}{{"assign", engine.Accept}, {"signal", engine.Ignore}, {"watch", engine.Ignore}} {
		ref := item(tc.event)
		receive(t, e, trigger(tc.event, tc.event, ref), engine.Stored)
		checkDue(t, e, 1)
		ev := held(t, s, ref)
		c, err := e.Candidate(t.Context(), ev.ID)
		must(t, err)
		out := engine.AssessByRules(c, p)
		if out.TriggerID != ev.Primary || out.EvalID != ev.ID || out.Kind != tc.want {
			t.Fatalf("规则结果 %+v", out)
		}
		d, err := e.Decide(t.Context(), out)
		must(t, err)
		if d.Kind != tc.want || (d.WorkID != "") != (tc.want == engine.Accept) {
			t.Fatalf("规则决策 %+v", d)
		}
		if tc.want == engine.Ignore && (d.Reason[0].Code != engine.ReasonObserved || string(d.Audit["would_accept"]) != "true" || len(d.Audit["proposal"]) == 0) {
			t.Fatalf("缺观察依据 %+v", d)
		}
	}
}

func reevaluate(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("retry")
	_, err := e.Reevaluate(ctx, "missing")
	checkCode(t, err, engine.CodeUnknownTrigger)
	for _, id := range []string{"one", "two"} {
		receive(t, e, trigger(id, "assign", ref), engine.Stored)
	}
	checkDue(t, e, 1)
	ev := held(t, s, ref)
	_, err = e.Decide(ctx, engine.Outcome{TriggerID: "one", EvalID: ev.ID, Kind: engine.Unavailable, Reason: engine.Reason{Code: engine.ReasonTimeout}})
	must(t, err)
	ok, err := e.Reevaluate(ctx, ref)
	must(t, err)
	if !ok || !slices.Equal(getThread(t, s, ref).Pending, ev.TriggerIDs) {
		t.Fatal("对象重试遗漏失败批次")
	}
	checkDue(t, e, 1)
	next := held(t, s, ref)
	if next.ID == ev.ID {
		t.Fatal("同毫秒重排复用了旧研判")
	}
	_, err = e.Decide(ctx, engine.Outcome{TriggerID: "one", EvalID: ev.ID, Kind: engine.Ignore})
	checkCode(t, err, engine.CodeEvalFinished)
	_, err = e.Decide(ctx, engine.Outcome{TriggerID: "two", EvalID: next.ID, Kind: engine.Ignore})
	must(t, err)
	ok, err = e.Reevaluate(ctx, ref)
	must(t, err)
	if ok {
		t.Fatal("已处理的失败被再次重排")
	}
	ok, err = e.Reevaluate(ctx, "one")
	must(t, err)
	if !ok || !slices.Equal(getThread(t, s, ref).Pending, []string{"one"}) {
		t.Fatal("指定事件重试失败")
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutDisposition("two", engine.TriggerDisposition{Recorded: true}) }))
	ok, err = e.Reevaluate(ctx, "two")
	must(t, err)
	if ok || len(getThread(t, s, ref).Pending) != 1 {
		t.Fatal("仅记录的事件被重排")
	}
}
