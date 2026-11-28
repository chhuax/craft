package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// RequestItem 以人的明确授权接下对象。已有正常在办订阅时返回对应工作，暂停期间也可调用。
func (e *Engine) RequestItem(ctx context.Context, ref ThreadRef, text string) (Work, error) {
	var result *Work
	key := ThreadKey(ref)
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		p, now := e.cfg.Policy(), e.cfg.Clock()
		th, err := u.GetThread(key)
		if err != nil {
			return err
		}
		th = threadOrNew(th, ref, now)
		if th.Closed != nil {
			return &UserError{Code: CodeNotAdmissible, Values: map[string]string{"reason": "对象已关闭"}}
		}
		works, err := u.GetWorks(th.Subscriptions...)
		if err != nil {
			return err
		}
		for _, id := range th.Subscriptions {
			if w := works[id]; w != nil && w.Ended == nil && !slices.Contains(th.Context, id) {
				result = w
				return nil
			}
		}
		// 退订只切断以后的消息，不能为同一份仍在办的起源再建一件工作。
		originID, err := u.OpenOriginWorkID(key)
		if err != nil {
			return err
		}
		if originID != "" {
			result = works[originID]
			if result == nil {
				result, err = u.GetWork(originID)
				if err != nil {
					return err
				}
			}
			if result == nil {
				return fmt.Errorf("起源索引引用的在办 Work %q 缺失", originID)
			}
			return nil
		}
		tr, err := pendingRequest(u, th)
		if err != nil {
			return err
		}
		if tr == nil {
			tr, err = requestTrigger(u, th, text, now)
			if err != nil {
				return err
			}
			if err := recordRequest(u, th, tr, works, now); err != nil {
				return err
			}
		}
		result, err = e.acceptRequest(u, th, tr, works, p, now)
		return err
	})
	if err != nil {
		return Work{}, fmt.Errorf("人工请求 Thread %q: %w", key, err)
	}
	return *result, nil
}

func (e *Engine) acceptRequest(u Unit, th *Thread, tr *Trigger, works map[string]*Work, p Policy, now string) (*Work, error) {
	key := th.Key
	evID := Key("eval", key, "request", tr.ID)
	if holding(th, tr.ID) {
		if len(th.Evaluating.TriggerIDs) == 1 {
			evID = th.Evaluating.ID
		} else {
			// 只接这条人请求；旧批次的其余事件仍须按原序独立研判。
			_, err := requeueBatch(u, th, Reason{Code: ReasonAcceptedBefore, TriggerID: tr.ID}, tr.ID, now)
			if err != nil {
				return nil, err
			}
		}
	}
	th.Pending = slices.DeleteFunc(th.Pending, func(id string) bool { return id == tr.ID })
	ev := Evaluation{ID: evID, ThreadKey: key, TriggerIDs: []string{tr.ID}, Primary: tr.ID,
		Strength: Explicit, Actor: tr.Actor, Basis: MandateBasis{Via: ViaPersonRequest, Source: tr.Source, Event: tr.Event, Mode: ModeAuto}, PolicyVersion: p.Version(), StartedAt: now}
	out := Outcome{Kind: Accept, TriggerID: tr.ID, Scope: tr.Text}
	_, w, err := e.admit(u, th, &ev, out, p, now, tr, works)
	return w, err
}

func pendingRequest(r Reader, th *Thread) (*Trigger, error) {
	ids := th.Pending
	if th.Evaluating != nil {
		ids = uniqueIDs(th.Evaluating.TriggerIDs, ids)
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := r.GetTriggers(ids...)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if tr := rows[id]; tr != nil && tr.Source == "craft" && tr.Event == "person.request" {
			return tr, nil
		}
	}
	return nil, nil
}

func recordRequest(u Unit, th *Thread, tr *Trigger, works map[string]*Work, now string) error {
	key := th.Key
	if err := u.PutTrigger(*tr); err != nil {
		return err
	}
	if err := u.LinkTrigger(tr.ID, key); err != nil {
		return err
	}
	th.LastAt = max(th.LastAt, now)
	d := TriggerDisposition{At: now}
	// 人请求直接走明确授权；原有只读订阅仍应收到普通接收路径会投递的副本。
	for _, id := range th.Context {
		w := works[id]
		if w == nil || w.Ended != nil {
			continue
		}
		if err := deliverInput(u, w, *tr, true, nil, now); err != nil {
			return err
		}
		d.Fanout = append(d.Fanout, id)
		d.Context = append(d.Context, id)
		if err := wake(u, w); err != nil {
			return err
		}
	}
	if err := u.PutDisposition(tr.ID, d); err != nil {
		return err
	}
	return emitReceived(u, tr.ID, []string{key}, now)
}

func requestTrigger(r Reader, th *Thread, text, now string) (*Trigger, error) {
	// 人可在同一毫秒内停止再发起，新的明确授权不能覆盖第一次的不可变事件。
	id := Key("request", th.Key, now)
	for {
		prev, err := r.GetTrigger(id)
		if err != nil {
			return nil, err
		}
		if prev == nil {
			break
		}
		id = Key("request", th.Key, now, id)
	}
	tr := &Trigger{ID: id, Source: "craft", Event: "person.request", Threads: []ThreadRef{th.Ref}, Actor: ActorRef{Source: "craft", ID: "person"}, Human: true, Addressed: "direct", Text: clipChars(text, 4000), At: now}
	if len(tr.Text) != len(text) {
		tr.Meta = map[string]json.RawMessage{"truncated": json.RawMessage("true")}
	}
	return tr, nil
}

// Dismiss 关闭一条不可用研判的待办，保留原有决策供审计。
func (e *Engine) Dismiss(ctx context.Context, id string) (Decision, error) {
	var d *Decision
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		var err error
		d, err = u.GetDecision(id)
		if err != nil {
			return err
		}
		if d == nil || d.Kind != Unavailable {
			return &UserError{Code: CodeUnknownProblem}
		}
		if d.Dismissed {
			return nil
		}
		d.Dismissed = true
		if err := u.PutDecision(*d); err != nil {
			return err
		}
		return emitAssessment(u, "decision.dismissed", e.cfg.Clock(), map[string]string{"decision": id, "thread": d.ThreadKey}, map[string]any{"decisionId": id, "threadKey": d.ThreadKey})
	})
	if err != nil {
		return Decision{}, fmt.Errorf("消除研判待办 %q: %w", id, err)
	}
	return *d, nil
}
