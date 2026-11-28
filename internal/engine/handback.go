package engine

import (
	"context"
	"fmt"
	"slices"
	"unicode/utf8"
)

// Handback 记录执行方退回事件的理由；最后一个认领者退回时才重新研判。
func (e *Engine) Handback(ctx context.Context, workID, triggerID, reason string) (bool, error) {
	queued := false
	_, err := e.withWork(ctx, "退回事件", workID, true, func(u Unit, w *Work, now string) error {
		if utf8.RuneCountInString(reason) > 300 {
			return fmt.Errorf("Trigger %q 的退回理由超过 300 字", triggerID)
		}
		origin := originIDs(w)
		triggers, err := u.GetTriggers(uniqueIDs(origin, []string{triggerID})...)
		if err != nil {
			return err
		}
		tr := triggers[triggerID]
		if tr == nil {
			return &UserError{Code: CodeUnknownTrigger}
		}
		if len(tr.Threads) == 0 {
			return fmt.Errorf("Trigger %q 缺少主对象", tr.ID)
		}
		threads, err := u.GetThreads(triggerThreadKeys(tr.Threads)...)
		if err != nil {
			return err
		}
		d, err := u.GetDisposition(tr.ID)
		if err != nil {
			return err
		}
		if d == nil {
			d = &TriggerDisposition{At: now}
		}
		if slices.ContainsFunc(d.Handbacks, func(h Handback) bool { return h.WorkID == w.ID }) {
			return &UserError{Code: CodeHandedBack}
		}
		if !eventReached(w, tr, d, threads) {
			return &UserError{Code: CodeNotReached}
		}
		others, err := otherRecipients(u, w, []*TriggerDisposition{d})
		if err != nil {
			return err
		}
		primary := threadOrNew(threads[ThreadKey(tr.Threads[0])], tr.Threads[0], now)
		queued, err = returnEvent(u, w, tr, primary, d, others, originRefs(origin, triggers), reason, now)
		return err
	})
	return queued && err == nil, err
}

func originIDs(w *Work) []string {
	if w.Origin.TriggerID == "" {
		return w.Origin.TriggerIDs
	}
	return uniqueIDs(w.Origin.TriggerIDs, []string{w.Origin.TriggerID})
}

func originRefs(ids []string, rows map[string]*Trigger) map[ThreadRef]bool {
	refs := make(map[ThreadRef]bool)
	for _, id := range ids {
		if tr := rows[id]; tr != nil {
			for _, ref := range tr.Threads {
				refs[ref] = true
			}
		}
	}
	return refs
}

func eventReached(w *Work, tr *Trigger, d *TriggerDisposition, threads map[string]*Thread) bool {
	if w.Origin.TriggerID == tr.ID || slices.Contains(w.Origin.TriggerIDs, tr.ID) || slices.Contains(d.Fanout, w.ID) {
		return true
	}
	if slices.ContainsFunc(w.Inputs, func(in WorkInput) bool { return in.Kind == "trigger" && in.TriggerID == tr.ID }) {
		return true
	}
	for _, th := range threads {
		if slices.Contains(th.Subscriptions, w.ID) {
			return true
		}
	}
	return false
}

// 当前工作已由命令读入；退回多个事件时，其他执行方也只批读一次。
func otherRecipients(r Reader, w *Work, disps []*TriggerDisposition) (map[string]*Work, error) {
	var ids []string
	for _, d := range disps {
		for _, id := range d.Fanout {
			if id != w.ID {
				ids = append(ids, id)
			}
		}
	}
	rows, err := r.GetWorks(uniqueIDs(ids)...)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		rows = map[string]*Work{}
	}
	rows[w.ID] = w
	return rows, nil
}

func returnEvent(u Unit, w *Work, tr *Trigger, th *Thread, d *TriggerDisposition, recipients map[string]*Work, sourceRefs map[ThreadRef]bool, reason, now string) (bool, error) {
	owners := workerOwners(tr, d, recipients)
	owned := slices.Contains(owners, w.ID)
	// attach 也能路由非人事件；它的自动来源仍应随退回撤销并重新研判。
	automatic, err := retractAutoFollow(u, w, tr, th, sourceRefs, reason, now)
	if err != nil {
		return false, err
	}
	if !tr.Human && !d.Requeued && !slices.Contains(w.Origin.TriggerIDs, tr.ID) && w.Origin.TriggerID != tr.ID {
		owned = automatic
		// 不可关闭对象没有 via，仍可从挂接决策区别被指派的事件与普通订阅副本。
		if !owned && d.DecisionID != "" {
			decision, err := u.GetDecision(d.DecisionID)
			if err != nil {
				return false, err
			}
			owned = decision != nil && slices.ContainsFunc(decision.Reason, func(r Reason) bool {
				return r.Code == ReasonRouted && r.TriggerID == tr.ID && r.Values["work"] == w.ID
			})
		}
	}
	d.Handbacks = append(d.Handbacks, Handback{WorkID: w.ID, Reason: reason, At: now})
	queued := owned && len(owners) <= 1 && !holding(th, tr.ID) && !slices.Contains(th.Pending, tr.ID)
	if queued {
		th.Pending = append(th.Pending, tr.ID)
		d.Requeued = true
		if err := u.PutThread(*th); err != nil {
			return false, err
		}
	}
	if err := u.PutDisposition(tr.ID, *d); err != nil {
		return false, err
	}
	return queued, emitAssessment(u, "trigger.handback", now, map[string]string{"work": w.ID, "trigger": tr.ID, "thread": th.Key}, map[string]any{"workId": w.ID, "triggerId": tr.ID, "reason": reason})
}

func retractAutoFollow(u Unit, w *Work, tr *Trigger, th *Thread, sourceRefs map[ThreadRef]bool, reason, now string) (bool, error) {
	i := followIndex(w, th.Key)
	if i < 0 || !slices.Contains(w.Follows[i].Via, tr.ID) {
		return false, nil
	}
	f := &w.Follows[i]
	f.Via = slices.DeleteFunc(f.Via, func(id string) bool { return id == tr.ID })
	if len(f.Via) == 0 && f.Manual != nil {
		f.Own = f.Manual.Own
	}
	if len(f.Via) == 0 && f.Manual == nil {
		w.Follows = slices.Delete(w.Follows, i, i+1)
		if !sourceRefs[th.Ref] {
			removeSubscription(th, w.ID)
			if err := u.PutThread(*th); err != nil {
				return false, err
			}
			if err := subscriptionEvent(u, "thread.unsubscribed", th.Key, w.ID, "executor", reason, now); err != nil {
				return false, err
			}
		}
	}
	w.UpdatedAt = now
	return true, u.PutWork(*w)
}

// MarkUnrelated 解除执行方与该对象的订阅，并交回它在该对象上仍负责的人事件。
func (e *Engine) MarkUnrelated(ctx context.Context, workID string, ref ThreadRef, reason string) error {
	_, err := e.withWork(ctx, "标记无关对象", workID, true, func(u Unit, w *Work, now string) error {
		key := ThreadKey(ref)
		inputIDs := []string{}
		for _, in := range w.Inputs {
			if in.Kind == "trigger" {
				inputIDs = append(inputIDs, in.TriggerID)
			}
		}
		ids := uniqueIDs(inputIDs)
		origin := originIDs(w)
		loadIDs := uniqueIDs(ids, origin)
		triggers, err := u.GetTriggers(loadIDs...)
		if err != nil {
			return err
		}
		refs := originRefs(origin, triggers)
		if refs[ref] {
			return &UserError{Code: CodeSourceThread}
		}
		i := followIndex(w, key)
		if i >= 0 {
			f := w.Follows[i]
			if f.Manual != nil || !f.Own || len(f.Via) == 0 {
				return &UserError{Code: CodeFollowedThread}
			}
		}
		th, err := u.GetThread(key)
		if err != nil {
			return err
		}
		if th == nil || !slices.Contains(th.Subscriptions, w.ID) {
			return &UserError{Code: CodeNotFollowing}
		}
		if i >= 0 {
			w.Follows = slices.Delete(w.Follows, i, i+1)
			w.UpdatedAt = now
			if err := u.PutWork(*w); err != nil {
				return err
			}
		}
		removeSubscription(th, w.ID)
		if err := u.PutThread(*th); err != nil {
			return err
		}
		if err := subscriptionEvent(u, "thread.unsubscribed", key, w.ID, "executor", reason, now); err != nil {
			return err
		}
		return returnRelated(u, w, th, ids, triggers, refs, reason, now)
	})
	return err
}

func returnRelated(u Unit, w *Work, th *Thread, ids []string, triggers map[string]*Trigger, refs map[ThreadRef]bool, reason, now string) error {
	var related, keys []string
	for _, id := range ids {
		tr := triggers[id]
		if tr == nil || !tr.Human || !slices.Contains(tr.Threads, th.Ref) {
			continue
		}
		related = append(related, id)
		if key := ThreadKey(tr.Threads[0]); key != th.Key {
			keys = append(keys, key)
		}
	}
	if len(related) == 0 {
		return nil
	}
	threads, err := u.GetThreads(uniqueIDs(keys)...)
	if err != nil {
		return err
	}
	if threads == nil {
		threads = make(map[string]*Thread)
	}
	threads[th.Key] = th
	disps, err := u.GetDispositions(related...)
	if err != nil {
		return err
	}
	var reached []*TriggerDisposition
	for _, id := range related {
		if d := disps[id]; d != nil {
			reached = append(reached, d)
		}
	}
	rows, err := otherRecipients(u, w, reached)
	if err != nil {
		return err
	}
	for _, id := range related {
		tr, d := triggers[id], disps[id]
		if !slices.Contains(workerOwners(tr, d, rows), w.ID) {
			continue
		}
		ref := tr.Threads[0]
		key := ThreadKey(ref)
		threads[key] = threadOrNew(threads[key], ref, now)
		if _, err := returnEvent(u, w, tr, threads[key], d, rows, refs, reason, now); err != nil {
			return err
		}
	}
	return nil
}
