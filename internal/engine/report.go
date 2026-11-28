package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// WorkerReport 是一次回报。HumanAction 的缺省、JSON null 和字符串分别代表保留、清除与新提问。
// Session 用于核对执行方身份。运行层自动回报须传代次，只有这条路径可报告 waiting。
type WorkerReport struct {
	State                ReportState     `json:"state"`
	Summary              string          `json:"summary"`
	HumanAction          json.RawMessage `json:"humanAction,omitempty"`
	Ask                  string          `json:"ask,omitempty"`
	ActionID             string          `json:"actionId,omitempty"`
	QuestionForTriggerID string          `json:"questionForTriggerId,omitempty"`
	ReplyToTriggerID     string          `json:"replyToTriggerId,omitempty"`
	Session              *Session        `json:"-"`
}

// Report 保存完整摘要。带代次的迟到回执返回 nil；会话命令在同一事务内验证当前绑定。
func (e *Engine) Report(ctx context.Context, workID, assignmentID string, req WorkerReport) (*Work, error) {
	var result *Work
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		w, err := u.GetWork(workID)
		if err != nil {
			return err
		}
		if req.Session != nil && !sessionMatches(w, req.Session) {
			return &UserError{Code: CodeNoWorkForSession}
		}
		if w == nil {
			if assignmentID != "" {
				return nil
			}
			return &UserError{Code: CodeUnknownWork}
		}
		a := currentAssignment(w)
		if assignmentID != "" && (a == nil || a.ID != assignmentID) {
			return nil
		}
		if w.Ended != nil {
			return &UserError{Code: CodeWorkEnded}
		}
		if len(w.Assignments) == 0 {
			return &UserError{Code: CodeUnknownAssignment}
		}
		if a == nil {
			return &UserError{Code: CodeAssignmentEnded}
		}
		if assignmentID == "" {
			return &UserError{Code: CodeUnknownAssignment}
		}
		switch req.State {
		case Waiting:
			if req.Session != nil {
				return fmt.Errorf("waiting 仅可由运行层按代次回报")
			}
		case Progress, Ready, Failed, Blocked, ReportDropped:
		default:
			return fmt.Errorf("未知报告状态 %q", req.State)
		}
		if err := recordReport(u, w, a, req, e.cfg.Clock()); err != nil {
			return err
		}
		result = w
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("派工 %q 回报 Work %q: %w", assignmentID, workID, err)
	}
	return result, nil
}

func recordReport(u Unit, w *Work, a *Assignment, req WorkerReport, now string) error {
	var previous string
	if a.Report != nil {
		previous = a.Report.ID
	}
	// 同毫秒可清除、重问或再报 ready；把前一份标记纳入新标记，旧卡片不能确认新的事实。
	id := Key("report", w.ID, a.ID, now, previous)
	if err := reportQuestion(u, w, req, id, now); err != nil {
		return err
	}
	if req.ReplyToTriggerID != "" {
		if err := personTarget(u, w, req.ReplyToTriggerID, false); err != nil {
			return err
		}
		if !slices.Contains(w.Answers, req.ReplyToTriggerID) {
			w.Answers = append(w.Answers, req.ReplyToTriggerID)
		}
	}
	a.Report = &Report{ID: id, State: req.State, Summary: req.Summary, At: now}
	w.UpdatedAt = now
	if req.State == Blocked || req.State == Failed {
		holdAutomatic(w, a)
	}
	if err := assignmentEvent(u, w, a, "assignment.reported", now, map[string]any{"state": req.State, "reportId": id}); err != nil {
		return err
	}
	return finishOrSave(u, w, now, req.State)
}

func reportQuestion(u Unit, w *Work, r WorkerReport, reportID, now string) error {
	var question *string
	if r.HumanAction != nil {
		if err := json.Unmarshal(r.HumanAction, &question); err != nil {
			return fmt.Errorf("解析 humanAction: %w", err)
		}
	}
	asking := question != nil && *question != ""
	ask := cmp.Or(r.Ask, "owner")
	if ask != "owner" && ask != "participants" {
		return fmt.Errorf("未知提问对象 %q", ask)
	}
	if asking && (r.State == Ready || r.ReplyToTriggerID != "") {
		return &UserError{Code: CodeQuestionWithResult}
	}
	if r.QuestionForTriggerID != "" {
		if !asking {
			return &UserError{Code: CodeAskerWithoutQuestion}
		}
		if err := personTarget(u, w, r.QuestionForTriggerID, true); err != nil {
			return err
		}
	}
	clear := r.State == Ready || (r.HumanAction != nil && question == nil)
	if !asking && !clear {
		return nil
	}
	if err := withdrawQuestion(u, w.Attention); err != nil {
		return err
	}
	w.Attention = nil
	if asking {
		w.Attention = &Question{ID: Key("attention", reportID), Question: *question, Ask: ask, ActionID: strings.TrimSpace(r.ActionID), TriggerID: r.QuestionForTriggerID, CreatedAt: now}
	}
	return nil
}

func withdrawQuestion(u Unit, q *Question) error {
	if q == nil {
		return nil
	}
	id := Key("attention-reply", q.ID)
	status, err := u.GetReplyStatus(id)
	if err != nil {
		return err
	}
	if status == ReplyPending || status == ReplyConfirm {
		if err := u.PutReplyStatus(id, ReplyDropped); err != nil {
			return err
		}
		// 覆盖正在预览的代次；即使旧 effect 已交给 outbox，也要唤醒渠道撤回待确认项。
		return enqueueEffect(u, "reply", id, OutboundMessage{ID: id})
	}
	return nil
}

func personTarget(r Reader, w *Work, id string, needChannel bool) error {
	tr, err := r.GetTrigger(id)
	if err != nil {
		return err
	}
	if tr != nil && tr.Human && (!needChannel || tr.Channel != nil) {
		ok, err := replyReached(r, w, id)
		if err != nil || ok {
			return err
		}
	}
	if needChannel {
		return &UserError{Code: CodeNotAsker}
	}
	return &UserError{Code: CodeNotAnswerable}
}

func replyReached(r Reader, w *Work, id string) (bool, error) {
	d, err := r.GetDisposition(id)
	if err != nil {
		return false, err
	}
	if d != nil {
		if slices.ContainsFunc(d.Handbacks, func(h Handback) bool { return h.WorkID == w.ID }) {
			return false, nil
		}
		if slices.Contains(d.Fanout, w.ID) {
			return !slices.Contains(d.Context, w.ID), nil
		}
	}
	if w.Origin.TriggerID == id || slices.Contains(w.Origin.TriggerIDs, id) {
		return true, nil
	}
	return slices.ContainsFunc(w.Inputs, func(in WorkInput) bool { return in.Kind == "trigger" && in.TriggerID == id && !in.Context }), nil
}

func holdAutomatic(w *Work, a *Assignment) {
	checking := w.DeliveryBatch
	if len(checking) == 0 && w.Delivery != nil && a.State == AssignmentRunning && len(w.Undelivered) > 0 {
		checking = w.Undelivered[:1]
	}
	drop := make(map[string]bool)
	for _, in := range w.Inputs {
		if in.Kind == "note" || in.Kind == "trigger" && !in.Human {
			drop[in.ID] = true
		}
	}
	for _, id := range checking {
		delete(drop, id)
	}
	w.Undelivered = slices.DeleteFunc(w.Undelivered, func(id string) bool { return drop[id] })
}
