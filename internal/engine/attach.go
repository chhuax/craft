package engine

import (
	"context"
	"fmt"
	"slices"
)

// Attach 把进行中批次的一条事件挂给已有授权的工作。批内其余事件回到队首。
func (e *Engine) Attach(ctx context.Context, workID, triggerID, note string) (Work, error) {
	var w *Work
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		p, now := e.cfg.Policy(), e.cfg.Clock()
		tr, err := requireTrigger(u, triggerID)
		if err != nil {
			return err
		}
		d, err := u.GetDisposition(triggerID)
		if err != nil {
			return err
		}
		if d == nil {
			d = &TriggerDisposition{At: now}
		}
		if slices.ContainsFunc(d.Handbacks, func(h Handback) bool { return h.WorkID == workID }) {
			return &UserError{Code: CodeHandedBackHere}
		}
		if err := leftToWorkers(u, tr, d); err != nil {
			return err
		}
		w, err = u.GetWork(workID)
		if err != nil {
			return err
		}
		if w == nil {
			return &UserError{Code: CodeUnknownWork}
		}
		if w.Ended != nil && w.Ended.By == "human" {
			return &UserError{Code: CodeWorkStopped}
		}
		if !p.Revalidate(w.Mandate) {
			return &UserError{Code: CodeMandateRevoked}
		}
		th, err := triggerThread(u, tr, now)
		if err != nil {
			return err
		}
		if !holding(th, triggerID) {
			return evalFinished(d.EvalID)
		}
		resuming := w.Ended != nil && tr.Human && p.ModeOf(tr.Source, tr.Event) != ModeOff
		threads, err := e.routeWork(u, w, th, []*Trigger{tr}, now, resuming)
		if err != nil {
			return err
		}
		attachInput(w, tr, d, note, now)
		d.Requeued = false
		d.Context = slices.DeleteFunc(d.Context, func(id string) bool { return id == workID })
		if !slices.Contains(d.Fanout, workID) {
			d.Fanout = append(d.Fanout, workID)
		}
		if err := u.PutDisposition(triggerID, *d); err != nil {
			return err
		}
		if err := saveRoutedInput(u, w, tr.ID, threads, now, resuming); err != nil {
			return err
		}
		reason := Reason{Code: ReasonRouted, Values: map[string]string{"work": workID}, TriggerID: triggerID}
		if len(th.Evaluating.TriggerIDs) == 1 {
			_, err = finishEval(u, th, th.Evaluating, Decision{Kind: Ignore, Reason: []Reason{reason}, At: now})
		} else {
			_, err = requeueBatch(u, th, reason, triggerID, now)
		}
		if err != nil {
			return err
		}
		if w.Ended != nil {
			return nil
		}
		return wake(u, w)
	})
	if err != nil {
		return Work{}, fmt.Errorf("挂接 Trigger %q 至 Work %q: %w", triggerID, workID, err)
	}
	return *w, nil
}

func attachInput(w *Work, tr *Trigger, d *TriggerDisposition, note, now string) {
	base := Key("input", w.ID, "trigger", tr.ID)
	id := base
	for i := range w.Inputs {
		in := &w.Inputs[i]
		if in.ID != base {
			continue
		}
		// 尚未送达的上下文副本可原地升级；已投递的历史保留原样，另登记此次路由。
		if note != "" && slices.Contains(w.Undelivered, base) {
			in.Context = false
			in.Note = note
			return
		}
		if d.Requeued || !slices.Contains(d.Fanout, w.ID) || slices.Contains(d.Context, w.ID) || w.Ended != nil {
			id = Key("input", w.ID, "trigger", tr.ID, "attached", d.EvalID)
		} else if note != "" {
			id = Key("input", w.ID, "trigger", tr.ID, "note", note)
		} else {
			return
		}
		if slices.ContainsFunc(w.Inputs, func(old WorkInput) bool { return old.ID == id }) {
			return
		}
		break
	}
	w.Inputs = append(w.Inputs, WorkInput{ID: id, Kind: "trigger", TriggerID: tr.ID, At: now, Note: note, Human: tr.Human})
	w.Undelivered = append(w.Undelivered, id)
}
