package engine

import (
	"context"
	"fmt"
	"slices"
)

// ReplyRequest 标识要答复的人的事件、要外发的当前问题或就绪结果。会话调用携带当前绑定。
type ReplyRequest struct {
	Kind         ReplyKind `json:"kind"`
	WorkID       string    `json:"workId,omitempty"`
	TriggerID    string    `json:"triggerId,omitempty"`
	AttentionID  string    `json:"attentionId,omitempty"`
	ReportID     string    `json:"reportId,omitempty"`
	Text         string    `json:"text"`
	Session      *Session  `json:"-"`
	AssignmentID string    `json:"assignmentId,omitempty"`
}

// ReplyReceipt 给出已入队消息的状态。Reused 表示已有同一消息，不会覆盖首次正文。
type ReplyReceipt struct {
	ID     string      `json:"id"`
	Status ReplyStatus `json:"status"`
	Reused bool        `json:"reused"`
}

// Reply 在一次事务中校验对象与渠道、登记消息回执并写入 reply effect。
func (e *Engine) Reply(ctx context.Context, req ReplyRequest) (ReplyReceipt, error) {
	var receipt ReplyReceipt
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		w, err := replyWork(u, req)
		if err != nil {
			return err
		}
		if req.Session != nil && (!sessionMatches(w, req.Session) || w.Ended != nil) {
			return &UserError{Code: CodeNoWorkForSession}
		}
		if req.Session != nil && req.AssignmentID == "" {
			return &UserError{Code: CodeUnknownAssignment}
		}
		if req.AssignmentID != "" {
			if a := currentAssignment(w); a == nil || a.ID != req.AssignmentID {
				return nil
			}
		}
		var tr *Trigger
		if req.TriggerID != "" {
			tr, err = requireTrigger(u, req.TriggerID)
			if err != nil {
				return err
			}
		}
		m, err := e.replyMessage(u, w, tr, req)
		if err != nil {
			return err
		}
		receipt, err = queueReply(u, m, w, tr, req, e.cfg.Clock())
		return err
	})
	if err != nil {
		return ReplyReceipt{}, fmt.Errorf("为 Work %q 外发 %s: %w", req.WorkID, req.Kind, err)
	}
	return receipt, nil
}

func (req ReplyRequest) answersTrigger() bool {
	return req.Kind == ReplyReply && req.AttentionID == "" && req.ReportID == "" && req.TriggerID != ""
}

func queueReply(u Unit, m OutboundMessage, w *Work, tr *Trigger, req ReplyRequest, now string) (ReplyReceipt, error) {
	status, err := u.GetReplyStatus(m.ID)
	if err != nil {
		return ReplyReceipt{}, err
	}
	if req.answersTrigger() && w != nil {
		if slices.Contains(w.Answers, tr.ID) {
			w.Answers = slices.DeleteFunc(w.Answers, func(id string) bool { return id == tr.ID })
			if err := u.PutWork(*w); err != nil {
				return ReplyReceipt{}, err
			}
		} else if status == "" {
			return ReplyReceipt{}, &UserError{Code: CodeNoAnswer}
		}
	}
	if m.Channel == nil {
		return ReplyReceipt{}, &UserError{Code: CodeNoChannel}
	}
	if status != "" {
		return ReplyReceipt{ID: m.ID, Status: status, Reused: true}, nil
	}
	if err := u.PutReplyStatus(m.ID, ReplyPending); err != nil {
		return ReplyReceipt{}, err
	}
	if err := emitEvent(u, "reply.queued", now, map[string]string{"work": m.WorkID, "message": m.ID}, map[string]any{"messageId": m.ID, "kind": m.Kind}); err != nil {
		return ReplyReceipt{}, err
	}
	if err := enqueueEffect(u, "reply", m.ID, m); err != nil {
		return ReplyReceipt{}, err
	}
	return ReplyReceipt{ID: m.ID, Status: ReplyPending}, nil
}

func replyWork(r Reader, req ReplyRequest) (*Work, error) {
	id := req.WorkID
	if id == "" && req.Kind == ReplyAcknowledgement {
		return nil, &UserError{Code: CodeUnknownWork}
	}
	if id == "" && req.TriggerID != "" {
		// 研判方可答尚未建单的来信；已有归属时只沿处置或接单索引寻址。
		d, err := r.GetDisposition(req.TriggerID)
		if err != nil {
			return nil, err
		}
		if d != nil {
			for _, work := range d.Fanout {
				if !slices.Contains(d.Context, work) && !slices.ContainsFunc(d.Handbacks, func(h Handback) bool { return h.WorkID == work }) {
					id = work
					break
				}
			}
		}
		if id == "" {
			rows, err := r.AcceptedByTrigger(req.TriggerID)
			if err != nil {
				return nil, err
			}
			id = rows[req.TriggerID].WorkID
		}
	}
	if id == "" {
		return nil, nil
	}
	w, err := r.GetWork(id)
	if err != nil {
		return nil, err
	}
	if w == nil {
		return nil, &UserError{Code: CodeUnknownWork}
	}
	return w, nil
}

func (e *Engine) replyMessage(r Reader, w *Work, tr *Trigger, req ReplyRequest) (OutboundMessage, error) {
	m := OutboundMessage{Kind: req.Kind, Text: req.Text}
	if w != nil {
		m.WorkID = w.ID
	}
	var address *ChannelRef
	switch {
	case req.Kind == ReplyAcknowledgement:
		if w.Ended != nil {
			return m, &UserError{Code: CodeAckEnded}
		}
		if tr == nil {
			return m, &UserError{Code: CodeUnknownTrigger}
		}
		ok, err := replyReached(r, w, tr.ID)
		if err != nil {
			return m, err
		}
		if !ok {
			return m, &UserError{Code: CodeNotReached}
		}
		if tr.Channel == nil {
			return m, &UserError{Code: CodeAckNoChannel}
		}
		if !tr.Human {
			return m, &UserError{Code: CodeAckNotPerson}
		}
		m.ID = Key("ack", tr.ID)
		address = tr.Channel
	case req.answersTrigger():
		if !tr.Human {
			return m, &UserError{Code: CodeNotQuestion}
		}
		if w != nil {
			ok, err := replyReached(r, w, tr.ID)
			if err != nil {
				return m, err
			}
			if !ok {
				return m, &UserError{Code: CodeNotReached}
			}
		}
		address = tr.Channel
		m.ID = Key("reply", tr.ID, req.Text)
	case req.Kind == ReplyReply:
		var err error
		m.ID, address, err = e.workMessage(r, w, req)
		if err != nil {
			return m, err
		}
	default:
		return m, fmt.Errorf("未知外发消息类型 %q", req.Kind)
	}
	m.Channel = address
	return m, nil
}

func (e *Engine) workMessage(r Reader, w *Work, req ReplyRequest) (string, *ChannelRef, error) {
	var askerID, id string
	if req.AttentionID != "" {
		if w == nil || w.Ended != nil || w.Attention == nil || w.Attention.ID != req.AttentionID || w.Attention.AnsweredAt != "" || w.Attention.Ask != "participants" {
			return "", nil, &UserError{Code: CodeNotAttention}
		}
		askerID = w.Attention.TriggerID
		id = Key("attention-reply", w.Attention.ID)
	} else {
		if !readyToSend(w, req.ReportID) {
			return "", nil, &UserError{Code: CodeNotReady}
		}
		id = Key("reply", w.ID, req.Text)
	}
	origin, err := r.GetTrigger(w.Origin.TriggerID)
	if err != nil {
		return "", nil, err
	}
	if askerID != "" && (origin == nil || len(origin.Threads) == 0 || e.cfg.Scope == nil || !e.cfg.Scope.Closable(origin.Threads[0].Source(), origin.Threads[0].Kind())) {
		asker, err := r.GetTrigger(askerID)
		if err != nil {
			return "", nil, err
		}
		if asker != nil && asker.Channel != nil {
			return id, asker.Channel, nil
		}
	}
	if origin != nil && origin.Channel != nil {
		return id, origin.Channel, nil
	}
	if len(w.Channels) > 0 {
		return id, &w.Channels[0], nil
	}
	return id, nil, nil
}

func readyToSend(w *Work, id string) bool {
	if w == nil {
		return false
	}
	var report *Report
	if n := len(w.Assignments); n > 0 {
		report = w.Assignments[n-1].Report
	}
	if id != "" && (report == nil || report.State != Ready || report.ID != id) {
		return false
	}
	if w.Ended == nil {
		return report != nil && report.State == Ready
	}
	switch w.Ended.Outcome {
	case Done, Completed, Dropped:
		return true
	}
	return false
}
