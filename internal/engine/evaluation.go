package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// EvaluateDue 按当前活跃集合逐对象提交。返回真正消费过 pending 的已提交对象数，
// 某个对象失败时保留其原队列，继续处理本轮其余对象。
func (e *Engine) EvaluateDue(ctx context.Context) (int, error) {
	var due []Thread
	err := e.cfg.Store.Read(ctx, func(r Reader) error {
		var err error
		due, err = r.ActiveThreadsWithPending()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("选择待研判对象: %w", err)
	}
	snapshot := sync.OnceValue(e.cfg.Policy)
	var failures []error
	count := 0
	for i := range due {
		key := due[i].Key
		var changed bool
		err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
			// 本轮共用首个事务取得的快照；决策落地时再核对版本。
			policy := snapshot()
			threads, err := u.GetThreads(key)
			if err != nil {
				return err
			}
			th := threads[key]
			if th == nil || th.Closed != nil || th.Evaluating != nil || len(th.Pending) == 0 {
				return nil
			}
			changed, err = startEvaluation(u, th, policy, e.cfg.Clock())
			return err
		})
		if err != nil {
			failures = append(failures, fmt.Errorf("研判 Thread %q: %w", key, err))
		} else if changed {
			count++
		}
	}
	return count, errors.Join(failures...)
}

// 归属取自实际分发处置。退回、人工重排和只读副本都不能代替当前研判作决定。
func workerOwners(tr *Trigger, d *TriggerDisposition, works map[string]*Work) []string {
	if !tr.Human || d == nil || d.Requeued {
		return nil
	}
	var owners []string
	for _, id := range d.Fanout {
		w := works[id]
		if w == nil || slices.Contains(d.Context, id) || slices.ContainsFunc(d.Handbacks, func(h Handback) bool { return h.WorkID == id }) {
			continue
		}
		if w.Ended == nil || (w.Ended.By != "human" && hasUndeliveredTrigger(w, tr.ID)) {
			owners = append(owners, id)
		}
	}
	return owners
}

func hasUndeliveredTrigger(w *Work, id string) bool {
	for i := range w.Inputs {
		in := &w.Inputs[i]
		if in.Kind == "trigger" && in.TriggerID == id && slices.Contains(w.Undelivered, in.ID) {
			return true
		}
	}
	return false
}

func worksInOrder(ids []string, rows map[string]*Work) []Work {
	works := make([]Work, 0, len(ids))
	for _, id := range ids {
		if w := rows[id]; w != nil {
			works = append(works, *w)
		}
	}
	return works
}

func startEvaluation(u Unit, th *Thread, p Policy, now string) (bool, error) {
	triggers, err := u.GetTriggers(th.Pending...)
	if err != nil {
		return false, err
	}
	var ids, waiting []string
	for _, id := range th.Pending {
		tr := triggers[id]
		if tr == nil {
			return false, fmt.Errorf("pending 引用的 Trigger %q 缺失", id)
		}
		if p.Paused() && (tr.Source != "craft" || tr.Event != "person.request") {
			waiting = append(waiting, id)
		} else {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return false, nil
	}
	th.Pending = waiting
	disps, err := u.GetDispositions(ids...)
	if err != nil {
		return false, err
	}
	workIDs := slices.Clone(th.Subscriptions)
	for _, id := range ids {
		if d := disps[id]; d != nil {
			workIDs = append(workIDs, d.Fanout...)
		}
	}
	works, err := u.GetWorks(workIDs...)
	if err != nil {
		return false, err
	}
	accepted, err := u.AcceptedByTrigger(ids...)
	if err != nil {
		return false, err
	}
	var batch, started []string
	var reasons []Reason
	for _, id := range ids {
		switch {
		case len(workerOwners(triggers[id], disps[id], works)) > 0:
			if err := emitTrigger(u, "trigger.followup", id, th.Key, now); err != nil {
				return false, err
			}
		case accepted[id].WorkID != "":
			started = append(started, id)
			reasons = append(reasons, startedReason(id, accepted[id].WorkID))
		default:
			batch = append(batch, id)
		}
	}
	if len(started) > 0 {
		if err := ignoreBatch(u, th, started, reasons, p, now, disps); err != nil {
			return false, err
		}
	}
	if len(batch) > 0 {
		return true, gateBatch(u, th, batch, triggers, disps, worksInOrder(th.Subscriptions, works), p, now)
	}
	return true, u.PutThread(*th)
}

func startedReason(id, work string) Reason {
	return Reason{Code: ReasonStartedWork, TriggerID: id, Values: map[string]string{"work": work}}
}

func gateBatch(u Unit, th *Thread, ids []string, triggers map[string]*Trigger, disps map[string]*TriggerDisposition, works []Work, p Policy, now string) error {
	var primary *Trigger
	var best GateResult
	var denied []Reason
	for _, id := range ids {
		tr := triggers[id]
		gate := p.Gate(*tr, *th, works)
		if !gate.Admissible {
			denied = append(denied, Reason{Code: gate.Reason, TriggerID: id})
			continue
		}
		if primary == nil || (gate.Strength == Explicit && best.Strength != Explicit) || (gate.Strength == best.Strength && tr.At >= primary.At) {
			best, primary = gate, tr
		}
	}
	if primary == nil {
		if err := ignoreBatch(u, th, ids, denied, p, now, disps); err != nil {
			return err
		}
		return u.PutThread(*th)
	}
	ev := Evaluation{ID: batchID(th.Key, "batch", p.Version(), now, ids, disps), ThreadKey: th.Key, TriggerIDs: ids, Primary: primary.ID,
		Strength: best.Strength, Actor: primary.Actor, Basis: best.Basis, PolicyVersion: p.Version(), StartedAt: now}
	th.Evaluating = &ev
	if err := u.PutThread(*th); err != nil {
		return err
	}
	if err := putEvalDispositions(u, &ev, "", now, disps); err != nil {
		return err
	}
	ref := map[string]string{"eval": ev.ID, "thread": th.Key, "trigger": ids[0]}
	if err := emitAssessment(u, "eval.started", now, ref, map[string]any{"evalId": ev.ID, "threadKey": th.Key, "triggerIds": ids, "strength": ev.Strength}); err != nil {
		return err
	}
	return enqueueAssessment(u, "assessment", ev.ID, map[string]string{"evalId": ev.ID})
}

func batchID(key, kind, version, now string, ids []string, disps map[string]*TriggerDisposition) string {
	// 同毫秒也可能完成后立刻重判。带上前一次研判，旧回执便无法命中新批次。
	parts := []string{"eval", key}
	if kind == "ignored" {
		parts = append(parts, kind)
	}
	parts = append(parts, version, now)
	first := Key(append(parts, ids...)...)
	for _, id := range ids {
		if d := disps[id]; d != nil && d.EvalID != "" {
			return Key("reassessment", first, d.EvalID)
		}
	}
	return first
}

func ignoreBatch(u Unit, th *Thread, ids []string, reason []Reason, p Policy, now string, disps map[string]*TriggerDisposition) error {
	ev := Evaluation{ID: batchID(th.Key, "ignored", p.Version(), now, ids, disps), ThreadKey: th.Key, TriggerIDs: ids, Strength: Inferred, PolicyVersion: p.Version()}
	_, err := recordDecision(u, &ev, Decision{Kind: Ignore, Reason: reason, At: now}, disps)
	return err
}
