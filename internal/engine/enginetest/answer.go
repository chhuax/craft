package enginetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func questionWork(t *testing.T, s engine.Store) engine.Work {
	t.Helper()
	w := assignmentFixture("owner")
	ref := item("shared")
	w.Origin = engine.Origin{TriggerID: "source"}
	w.Follows = []engine.Follow{{Own: true, Thread: engine.ThreadKey(ref)}}
	tr := trigger("source", "message", ref)
	tr.Human = true
	tr.Channel = &engine.ChannelRef{Source: "feed", Thread: json.RawMessage(`"room"`)}
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutTrigger(tr))
		must(t, u.PutThread(engine.Thread{Key: engine.ThreadKey(ref), Ref: ref, Subscriptions: []string{w.ID, "other"}}))
		must(t, u.PutWork(assignmentFixture("other")))
		return u.PutWork(w)
	}))
	return w
}

func answerCommand(t *testing.T, s engine.Store) {
	ctx := t.Context()
	w := questionWork(t, s)
	var calls int
	cfg := config(s)
	cfg.Policy = func() engine.Policy { calls++; return engine.Policy{} }
	e := newEngine(t, cfg)
	_, err := e.Answer(ctx, w.ID, "提前")
	checkCode(t, err, engine.CodeNoAttention)
	report := engine.WorkerReport{State: engine.Blocked, HumanAction: json.RawMessage(`"继续吗"`), Ask: "participants", ActionID: "  continue ", QuestionForTriggerID: "source"}
	asked, err := e.Report(ctx, w.ID, "a", report)
	must(t, err)
	if asked.Attention.Ask != "participants" || asked.Attention.ActionID != "continue" || asked.Attention.TriggerID != "source" || engine.WorkStage(*asked).Action != "answer" {
		t.Fatal(asked.Attention)
	}
	answer, err := e.Answer(ctx, w.ID, "继续")
	must(t, err)
	if calls != 3 {
		t.Fatalf("三条命令取策略 %d 次", calls)
	}
	if answer.Attention.AnsweredAt != now || engine.WorkStage(answer).Stage != "working" || len(answer.Inputs) != 1 || !answer.Inputs[0].Human {
		t.Fatal("回答与待办未联动")
	}
	tid := answer.Inputs[0].TriggerID
	k := engine.ThreadKey(item("shared"))
	st := readIntake(t, s, tid, []string{k}, []string{"other"})
	if st.trigger == nil || st.trigger.Source != "craft" || st.trigger.Event != "person.answer" || st.trigger.Addressed != "direct" || st.trigger.Text != "继续" || !slices.Equal(st.disposition.Fanout, []string{w.ID}) || len(st.works["other"].Inputs) != 0 || len(st.threads[k].Pending) != 0 || !slices.Equal(st.keys, []string{k}) {
		t.Fatalf("回答路由错误 %+v", st)
	}
	_, err = e.Answer(ctx, w.ID, "继续")
	must(t, err)
	if len(readWork(t, s, w.ID).Inputs) != 1 {
		t.Fatal("同一回答重复入队")
	}
	kept, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Progress})
	must(t, err)
	if kept.Attention.AnsweredAt != now {
		t.Fatal("省略问题时丢失回答")
	}
	reasked, err := e.Report(ctx, w.ID, "a", report)
	must(t, err)
	if reasked.Attention.ID == asked.Attention.ID || reasked.Attention.AnsweredAt != "" {
		t.Fatal("重问沿用了原问题")
	}
	again, err := e.Answer(ctx, w.ID, "继续")
	must(t, err)
	if len(again.Inputs) != 2 || again.Inputs[1].TriggerID == tid {
		t.Fatal("新问题的同文回答被误合并")
	}
	cleared, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Progress, HumanAction: json.RawMessage("null")})
	must(t, err)
	if cleared.Attention != nil {
		t.Fatal("显式 null 未清除问题")
	}
	for _, tc := range []struct{ by, code string }{{"executor", engine.CodeWorkEnded}, {"human", engine.CodeWorkStopped}} {
		w.Ended = &engine.WorkEnd{By: tc.by}
		saveWork(t, s, w)
		_, err = e.Answer(ctx, w.ID, "回答")
		checkCode(t, err, tc.code)
	}
}

func reportRefusals(t *testing.T, s engine.Store) {
	ctx := t.Context()
	w := questionWork(t, s)
	e := newEngine(t, config(s))
	for _, tc := range []struct {
		req  engine.WorkerReport
		code string
	}{
		{engine.WorkerReport{State: engine.Ready, HumanAction: json.RawMessage(`"问题"`)}, engine.CodeQuestionWithResult},
		{engine.WorkerReport{State: engine.Progress, HumanAction: json.RawMessage(`"问题"`), ReplyToTriggerID: "source"}, engine.CodeQuestionWithResult},
		{engine.WorkerReport{State: engine.Progress, QuestionForTriggerID: "source"}, engine.CodeAskerWithoutQuestion},
		{engine.WorkerReport{State: engine.Progress, QuestionForTriggerID: "elsewhere", HumanAction: json.RawMessage(`"问题"`)}, engine.CodeNotAsker},
		{engine.WorkerReport{State: engine.Progress, ReplyToTriggerID: "elsewhere"}, engine.CodeNotAnswerable},
		{engine.WorkerReport{State: engine.Progress, Session: &engine.Session{Host: "local", ID: "old"}}, engine.CodeNoWorkForSession},
	} {
		_, err := e.Report(ctx, w.ID, "a", tc.req)
		checkCode(t, err, tc.code)
	}
	for _, tc := range []struct {
		a    []engine.Assignment
		end  *engine.WorkEnd
		code string
	}{
		{nil, nil, engine.CodeUnknownAssignment}, {[]engine.Assignment{{ID: "old", State: engine.AssignmentEnded}}, nil, engine.CodeAssignmentEnded}, {nil, &engine.WorkEnd{}, engine.CodeWorkEnded},
	} {
		w.Assignments = tc.a
		w.Ended = tc.end
		saveWork(t, s, w)
		_, err := e.Report(ctx, w.ID, "", engine.WorkerReport{State: engine.Progress})
		checkCode(t, err, tc.code)
	}
}

func commandRollback(t *testing.T, s engine.Store) {
	w := questionWork(t, s)
	w.Attention = &engine.Question{ID: "q", Question: "继续吗"}
	w.Answers = []string{"source"}
	w.Inputs = []engine.WorkInput{{ID: "unread", Kind: "trigger", TriggerID: "source", Human: true}}
	saveWork(t, s, w)
	ctx := t.Context()
	e := newEngine(t, config(&queueStore{Store: s, check: func(_, _ string, _ json.RawMessage) error { return errQueue }}))
	tid := engine.Key("answer", w.ID, "q", now, "继续")
	var broadcasts int
	s.Subscribe(func(engine.Event) { broadcasts++ })
	before := readIntake(t, s, tid, []string{engine.ThreadKey(item("shared"))}, []string{w.ID, "other"})
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"报告", func() error {
			_, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.ReportDropped})
			return err
		}},
		{"回答", func() error { _, err := e.Answer(ctx, w.ID, "继续"); return err }},
		{"回复", func() error {
			_, err := e.Reply(ctx, engine.ReplyRequest{Kind: engine.ReplyReply, WorkID: w.ID, TriggerID: "source", Text: "答复"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, errQueue) {
				t.Fatal(err)
			}
			if broadcasts != 0 || !reflect.DeepEqual(before, readIntake(t, s, tid, []string{engine.ThreadKey(item("shared"))}, []string{w.ID, "other"})) {
				t.Fatal("入队失败仍留下了状态、输入或广播")
			}
		})
	}
}
