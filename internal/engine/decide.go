package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
)

func requireTrigger(r Reader, id string) (*Trigger, error) {
	tr, err := r.GetTrigger(id)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		return nil, &UserError{Code: CodeUnknownTrigger}
	}
	if len(tr.Threads) == 0 {
		return nil, fmt.Errorf("Trigger %q 缺少主对象", id)
	}
	return tr, nil
}

func triggerThread(r Reader, tr *Trigger, now string) (*Thread, error) {
	ref := tr.Threads[0]
	th, err := r.GetThread(ThreadKey(ref))
	if err != nil {
		return nil, err
	}
	return threadOrNew(th, ref, now), nil
}

func holding(th *Thread, id string) bool {
	return th.Evaluating != nil && slices.Contains(th.Evaluating.TriggerIDs, id)
}

func evalFinished(id string) error {
	return &UserError{Code: CodeEvalFinished, Values: map[string]string{"eval": id}}
}

func leftToWorkers(r Reader, tr *Trigger, d *TriggerDisposition) error {
	if !tr.Human || d == nil || d.Requeued || len(d.Fanout) == 0 {
		return nil
	}
	works, err := r.GetWorks(d.Fanout...)
	if err != nil {
		return err
	}
	if owners := workerOwners(tr, d, works); len(owners) > 0 {
		return &UserError{Code: CodeWorkerDecides, Values: map[string]string{"works": strings.Join(owners, "、")}}
	}
	return nil
}

// Decide 在一个事务中核对回执、落决策并登记派工。拒绝或入队失败时保留原研判。
func (e *Engine) Decide(ctx context.Context, out Outcome) (Decision, error) {
	var d Decision
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		var err error
		d, err = e.decide(u, out, e.cfg.Policy(), e.cfg.Clock())
		return err
	})
	if err != nil {
		return Decision{}, fmt.Errorf("决定 Trigger %q: %w", out.TriggerID, err)
	}
	return d, nil
}

func (e *Engine) decide(u Unit, out Outcome, p Policy, now string) (Decision, error) {
	tr, err := requireTrigger(u, out.TriggerID)
	if err != nil {
		return Decision{}, err
	}
	th, err := triggerThread(u, tr, now)
	if err != nil {
		return Decision{}, err
	}
	var ev *Evaluation
	if holding(th, tr.ID) {
		ev = th.Evaluating
	}
	if out.EvalID != "" && ev != nil && ev.ID != out.EvalID {
		return Decision{}, evalFinished(out.EvalID)
	}
	if out.Kind == Accept {
		old, err := priorAcceptance(u, tr.ID)
		if err != nil {
			return Decision{}, err
		}
		if old != nil && (out.EvalID == "" || ev != nil || old.EvalID == out.EvalID) {
			// 同一份已提交的 accept 回执可安全重放，其他过期代次仍按 evalFinished 拒绝。
			if ev != nil {
				if err := finishDuplicate(u, th, tr.ID, old.WorkID, now); err != nil {
					return Decision{}, err
				}
			}
			return *old, nil
		}
	}
	if out.EvalID != "" && ev == nil {
		return Decision{}, evalFinished(out.EvalID)
	}
	d, err := u.GetDisposition(tr.ID)
	if err != nil {
		return Decision{}, err
	}
	if out.Kind != Stop {
		if err := leftToWorkers(u, tr, d); err != nil {
			return Decision{}, err
		}
	}
	if ev == nil {
		ev, err = directEval(u, th, tr, d, out, p, now)
		if err != nil {
			return Decision{}, err
		}
	}
	if ev.PolicyVersion != p.Version() {
		return requeueBatch(u, th, Reason{Code: ReasonPolicyChanged}, "", now)
	}
	switch out.Kind {
	case Accept:
		decision, _, err := e.admit(u, th, ev, out, p, now, tr, nil)
		return decision, err
	case Stop:
		return decideStop(u, th, ev, out, now)
	case Ignore, Unavailable:
		return finishEval(u, th, ev, Decision{Kind: out.Kind, At: now, Reason: outcomeReasons(out, ev), Audit: out.Audit})
	default:
		return Decision{}, fmt.Errorf("未知研判结论 %q", out.Kind)
	}
}

func priorAcceptance(r Reader, id string) (*Decision, error) {
	found, err := r.AcceptedByTrigger(id)
	if err != nil {
		return nil, err
	}
	if prior, ok := found[id]; ok {
		return r.GetDecision(prior.DecisionID)
	}
	return nil, nil
}

func finishDuplicate(u Unit, th *Thread, id, workID, now string) error {
	if len(th.Evaluating.TriggerIDs) > 1 {
		_, err := requeueBatch(u, th, Reason{Code: ReasonAcceptedBefore, Values: map[string]string{"trigger": id}}, id, now)
		return err
	}
	_, err := finishEval(u, th, th.Evaluating, Decision{Kind: Ignore, At: now, Reason: []Reason{startedReason(id, workID)}})
	return err
}

func directEval(u Unit, th *Thread, tr *Trigger, d *TriggerDisposition, out Outcome, p Policy, now string) (*Evaluation, error) {
	works, err := u.GetWorks(th.Subscriptions...)
	if err != nil {
		return nil, err
	}
	gate := p.Gate(*tr, *th, worksInOrder(th.Subscriptions, works))
	if !gate.Admissible {
		return nil, &UserError{Code: CodeNotAdmissible, Values: map[string]string{"reason": gate.Reason}}
	}
	th.Pending = slices.DeleteFunc(th.Pending, func(id string) bool { return id == tr.ID })
	parts := []string{"eval", th.Key, "direct", tr.ID, string(out.Kind)}
	if out.Kind == Stop && out.WorkID != "" {
		parts = append(parts, out.WorkID)
	}
	id := Key(append(parts, now)...)
	if d != nil && d.EvalID != "" {
		id = Key("reassessment", id, d.EvalID)
	}
	return &Evaluation{ID: id, ThreadKey: th.Key, TriggerIDs: []string{tr.ID}, Primary: tr.ID, Strength: gate.Strength, Basis: gate.Basis, Actor: tr.Actor, PolicyVersion: p.Version(), StartedAt: now}, nil
}

func decideStop(u Unit, th *Thread, ev *Evaluation, out Outcome, now string) (Decision, error) {
	target, err := u.GetWork(out.WorkID)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Kind: Stop, At: now, Audit: out.Audit, Reason: outcomeReasons(out, ev)}
	switch {
	case target == nil || target.Ended != nil:
		d.Kind = Ignore
		d.Reason = []Reason{{Code: ReasonStopUnknown}}
	case ev.Strength != Explicit:
		d.Kind = Ignore
		d.Reason = []Reason{{Code: ReasonStopNotExplicit}}
	default:
		d.WorkID = target.ID
	}
	return finishEval(u, th, ev, d)
}

func (e *Engine) admit(u Unit, th *Thread, ev *Evaluation, out Outcome, p Policy, now string, tr *Trigger, knownWorks map[string]*Work) (Decision, *Work, error) {
	admission := p.Admits(*ev, out.Rules)
	if admission == AdmitRefuse {
		return Decision{}, nil, &UserError{Code: CodeNotExplicit}
	}
	ids := slices.DeleteFunc(slices.Clone(ev.TriggerIDs), func(id string) bool { return id == tr.ID })
	triggers, err := u.GetTriggers(ids...)
	if err != nil {
		return Decision{}, nil, err
	}
	if triggers == nil {
		triggers = make(map[string]*Trigger)
	}
	triggers[tr.ID] = tr
	primary := triggers[primaryID(ev)]
	if primary == nil {
		return Decision{}, nil, fmt.Errorf("研判 %q 缺少主事件", ev.ID)
	}
	title := assessmentTitle(th, primary.Text)
	out.Scope = cmp.Or(out.Scope, title)
	if admission == AdmitObserve {
		out = observed(out, title, th.Key)
		d, err := finishEval(u, th, ev, Decision{Kind: Ignore, At: now, Reason: []Reason{out.Reason}, Audit: out.Audit})
		return d, nil, err
	}
	w := Work{ID: Key("work", ev.ID), Title: title, Mandate: mandateOf(ev, out), CreatedAt: now, UpdatedAt: now, Origin: Origin{TriggerID: primary.ID, TriggerIDs: ev.TriggerIDs}}
	target := out.Target
	if target == nil {
		target = &TargetSnapshot{ID: "scratch", Kind: "scratch", Title: title, Evidence: []string{"v1 无选址，默认在临时目录执行"}, SelectedBy: "default"}
	}
	w.Execution = &Execution{Target: target}
	batch := make([]*Trigger, 0, len(ev.TriggerIDs))
	for _, id := range ev.TriggerIDs {
		tr := triggers[id]
		if tr == nil || len(tr.Threads) == 0 {
			return Decision{}, nil, fmt.Errorf("批内事件 %q 缺失或无对象", id)
		}
		batch = append(batch, tr)
	}
	if _, err := e.routeWork(u, &w, th, batch, now, false, knownWorks); err != nil {
		return Decision{}, nil, err
	}
	d, err := finishEval(u, th, ev, Decision{At: now, Kind: Accept, Reason: outcomeReasons(out, ev), Audit: out.Audit, WorkID: w.ID, Mandate: &w.Mandate})
	if err != nil {
		return Decision{}, nil, err
	}
	w.DecisionID = d.ID
	if err := launchWork(u, &w, batch, now); err != nil {
		return Decision{}, nil, err
	}
	return d, &w, nil
}
