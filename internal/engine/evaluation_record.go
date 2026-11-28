package engine

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
)

func emitAssessment(u Unit, kind, now string, ref map[string]string, fields map[string]any) error {
	data := make(map[string]json.RawMessage, len(fields))
	for name, v := range fields {
		raw, err := jsonv2.Marshal(v)
		if err != nil {
			return fmt.Errorf("编码事件 %s 的 %s: %w", kind, name, err)
		}
		data[name] = raw
	}
	return u.Emit(Event{Type: kind, At: now, Ref: ref, Data: data})
}

func emitTrigger(u Unit, kind, id, key, now string) error {
	return emitAssessment(u, kind, now, map[string]string{"thread": key, "trigger": id}, map[string]any{"triggerId": id})
}

func enqueueAssessment(u Unit, kind, key string, data any) error {
	raw, err := jsonv2.Marshal(data)
	if err != nil {
		return fmt.Errorf("编码 %s 请求: %w", kind, err)
	}
	if err := u.Effects().Enqueue(kind, key, raw); err != nil {
		return fmt.Errorf("登记 %s 请求 %q: %w", kind, key, err)
	}
	return nil
}

func putEvalDispositions(u Unit, ev *Evaluation, decisionID, now string, disps map[string]*TriggerDisposition) error {
	for _, id := range ev.TriggerIDs {
		d := disps[id]
		if d == nil {
			d = &TriggerDisposition{At: now}
		}
		d.EvalID = ev.ID
		if decisionID != "" {
			d.DecisionID = decisionID
		}
		if err := u.PutDisposition(id, *d); err != nil {
			return err
		}
	}
	return nil
}

func recordDecision(u Unit, ev *Evaluation, d Decision, disps map[string]*TriggerDisposition) (Decision, error) {
	d.ID = Key("decision", ev.ID)
	d.EvalID = ev.ID
	d.ThreadKey = ev.ThreadKey
	d.TriggerIDs = ev.TriggerIDs
	d.PolicyVersion = ev.PolicyVersion
	d.Strength = ev.Strength
	if err := u.PutDecision(d); err != nil {
		return Decision{}, err
	}
	if err := putEvalDispositions(u, ev, d.ID, d.At, disps); err != nil {
		return Decision{}, err
	}
	ref := map[string]string{"decision": d.ID, "eval": ev.ID, "thread": ev.ThreadKey}
	if len(ev.TriggerIDs) > 0 {
		ref["trigger"] = ev.TriggerIDs[0]
	}
	if d.WorkID != "" {
		ref["work"] = d.WorkID
	}
	err := emitAssessment(u, "decision.recorded", d.At, ref, map[string]any{"decisionId": d.ID, "threadKey": ev.ThreadKey, "kind": d.Kind})
	return d, err
}

func finishEval(u Unit, th *Thread, ev *Evaluation, d Decision) (Decision, error) {
	disps, err := u.GetDispositions(ev.TriggerIDs...)
	if err != nil {
		return Decision{}, err
	}
	d, err = recordDecision(u, ev, d, disps)
	if err != nil {
		return Decision{}, err
	}
	// 直接决定另一条事件时，同一对象原有的研判仍须保留。
	if th.Evaluating != nil && th.Evaluating.ID == ev.ID {
		th.Evaluating = nil
	}
	return d, u.PutThread(*th)
}
