package enginetest

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func replyCommands(t *testing.T, s engine.Store) {
	ctx := t.Context()
	w := questionWork(t, s)
	var msgs []engine.OutboundMessage
	store := &queueStore{Store: s, check: func(kind, key string, raw json.RawMessage) error {
		var m engine.OutboundMessage
		must(t, json.Unmarshal(raw, &m))
		if kind != "reply" || m.ID != key || m.WorkID != w.ID || string(m.Channel.Thread) != `"room"` {
			t.Fatalf("回复载荷 %s/%s %s", kind, key, raw)
		}
		msgs = append(msgs, m)
		return nil
	}}
	e := newEngine(t, config(store))
	tr := trigger("robot", "change", item("shared"))
	w.Inputs = []engine.WorkInput{{ID: "i", Kind: "trigger", TriggerID: "robot"}, {ID: "j", Kind: "trigger", TriggerID: "nochannel", Human: true}, {ID: "k", Kind: "trigger", TriggerID: "context", Human: true, Context: true}}
	tr.Channel = &engine.ChannelRef{Source: "feed", Thread: json.RawMessage(`"room"`)}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutTrigger(tr))
		tr.Human = true
		tr.ID = "context"
		must(t, u.PutTrigger(tr))
		tr.ID = "nochannel"
		tr.Channel = nil
		must(t, u.PutTrigger(tr))
		return u.PutWork(w)
	}))
	for _, tc := range []struct {
		kind                      engine.ReplyKind
		id, work, attention, code string
	}{
		{"acknowledgement", "absent", w.ID, "", engine.CodeUnknownTrigger}, {"acknowledgement", "context", w.ID, "", engine.CodeNotReached}, {"acknowledgement", "source", "other", "", engine.CodeNotReached},
		{"acknowledgement", "nochannel", w.ID, "", engine.CodeAckNoChannel}, {"acknowledgement", "robot", w.ID, "", engine.CodeAckNotPerson}, {"acknowledgement", "source", "absent", "", engine.CodeUnknownWork},
		{"reply", "robot", w.ID, "", engine.CodeNotQuestion}, {"reply", "source", w.ID, "", engine.CodeNoAnswer}, {"reply", "", "other", "stale", engine.CodeNotAttention}, {"reply", "", w.ID, "", engine.CodeNotReady},
	} {
		_, err := e.Reply(ctx, engine.ReplyRequest{Kind: tc.kind, TriggerID: tc.id, WorkID: tc.work, AttentionID: tc.attention, Text: "回复"})
		checkCode(t, err, tc.code)
	}
	req := engine.ReplyRequest{Kind: engine.ReplyAcknowledgement, WorkID: w.ID, TriggerID: "source", Text: "收到"}
	ack, err := e.Reply(ctx, req)
	must(t, err)
	if ack.Reused || ack.Status != engine.ReplyPending || ack.ID != engine.Key("ack", "source") {
		t.Fatal(ack)
	}
	req.Text = "重复"
	for _, status := range []engine.ReplyStatus{engine.ReplyPending, engine.ReplyConfirm, engine.ReplySent, engine.ReplyDropped} {
		must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutReplyStatus(ack.ID, status) }))
		again, err := e.Reply(ctx, req)
		must(t, err)
		if !again.Reused || again.Status != status || again.ID != ack.ID || len(msgs) != 1 || msgs[0].Text != "收到" {
			t.Fatal("重复确认覆盖了首次消息或状态")
		}
	}
	for _, id := range []string{"source", "nochannel"} {
		_, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Progress, ReplyToTriggerID: id})
		must(t, err)
	}
	req = engine.ReplyRequest{Kind: engine.ReplyReply, WorkID: w.ID, TriggerID: "nochannel", Text: "答复"}
	_, err = e.Reply(ctx, req)
	checkCode(t, err, engine.CodeNoChannel)
	req.TriggerID = "source"
	reply, err := e.Reply(ctx, req)
	must(t, err)
	again, err := e.Reply(ctx, req)
	must(t, err)
	if again.ID != reply.ID || !again.Reused || len(msgs) != 2 || msgs[1].Kind != "reply" || slices.Contains(readWork(t, s, w.ID).Answers, "source") {
		t.Fatal("答复未去重或未消费已回答记录")
	}
	req.Text = "另一个答复"
	_, err = e.Reply(ctx, req)
	checkCode(t, err, engine.CodeNoAnswer)
	w = readWork(t, s, w.ID)
	w.Ended = &engine.WorkEnd{By: "human", Outcome: engine.Stopped}
	saveWork(t, s, w)
	_, err = e.Reply(ctx, engine.ReplyRequest{Kind: engine.ReplyAcknowledgement, WorkID: w.ID, TriggerID: "source"})
	checkCode(t, err, engine.CodeAckEnded)
}

func guardedReplies(t *testing.T, s engine.Store) {
	w := questionWork(t, s)
	ctx := t.Context()
	e := newEngine(t, config(s))
	q := engine.WorkerReport{State: engine.Progress, HumanAction: json.RawMessage(`"是否继续"`), Ask: "participants", QuestionForTriggerID: "source"}
	asked, err := e.Report(ctx, w.ID, "a", q)
	must(t, err)
	req := engine.ReplyRequest{Kind: engine.ReplyReply, WorkID: w.ID, AttentionID: asked.Attention.ID, Text: "是否继续"}
	first, err := e.Reply(ctx, req)
	must(t, err)
	next, err := e.Report(ctx, w.ID, "a", q)
	must(t, err)
	if next.Attention.ID == asked.Attention.ID {
		t.Fatal("同毫秒的问题复用了旧标记")
	}
	_, err = e.Reply(ctx, req)
	checkCode(t, err, engine.CodeNotAttention)
	must(t, s.Read(ctx, func(r engine.Reader) error {
		state, err := r.GetReplyStatus(first.ID)
		must(t, err)
		if state != engine.ReplyDropped {
			t.Fatal("旧问题仍待外发")
		}
		return nil
	}))
	req.AttentionID = next.Attention.ID
	req.Session = &engine.Session{ID: "previous", Host: "local"}
	_, err = e.Reply(ctx, req)
	checkCode(t, err, engine.CodeNoWorkForSession)
	result, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Ready, Summary: "完成"})
	must(t, err)
	id := result.Assignments[0].Report.ID
	req = engine.ReplyRequest{Kind: engine.ReplyReply, WorkID: w.ID, ReportID: id, Text: "完成"}
	out, err := e.Reply(ctx, req)
	must(t, err)
	if out.Status != engine.ReplyPending {
		t.Fatal(out)
	}
	_, err = e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Ready, Summary: "再次完成"})
	must(t, err)
	_, err = e.Reply(ctx, req)
	checkCode(t, err, engine.CodeNotReady)
	ask := engine.ReplyRequest{Kind: engine.ReplyReply, TriggerID: "unassigned", Text: "答复"}
	tr := trigger("unassigned", "ask", item("new"))
	tr.Channel = &engine.ChannelRef{Source: "feed", Thread: json.RawMessage(`"new"`)}
	tr.Human = true
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutTrigger(tr) }))
	got, err := e.Reply(ctx, ask)
	must(t, err)
	if got.Reused {
		t.Fatal("无 Work 的来信未由研判方回复")
	}
}
