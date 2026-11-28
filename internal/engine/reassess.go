package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
)

// ReassessEval 把指定的活跃批次放回队首。仅在实际重排时返回 true。
// 普通重试不生成失败决策；应用层用 Decide(unavailable) 终结耗尽重试的批次。
func (e *Engine) ReassessEval(ctx context.Context, evalID string) (bool, error) {
	changed := false
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		th, err := u.ThreadByEval(evalID)
		if err != nil || th == nil {
			return err
		}
		ids := th.Evaluating.TriggerIDs
		prependBatch(th, "")
		th.Evaluating = nil
		if err := u.PutThread(*th); err != nil {
			return err
		}
		now := e.cfg.Clock()
		for _, id := range ids {
			if err := emitTrigger(u, "trigger.requeued", id, th.Key, now); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("重排研判 %q: %w", evalID, err)
	}
	return changed, nil
}

func requeueBatch(u Unit, th *Thread, reason Reason, without, now string) (Decision, error) {
	prependBatch(th, without)
	return finishEval(u, th, th.Evaluating, Decision{Kind: Unavailable, At: now, Reason: []Reason{reason}})
}

func prependBatch(th *Thread, without string) {
	ev := th.Evaluating
	pending := make([]string, 0, len(ev.TriggerIDs)+len(th.Pending))
	seen := make(map[string]bool, len(ev.TriggerIDs))
	for _, id := range ev.TriggerIDs {
		seen[id] = true
		if id != without {
			pending = append(pending, id)
		}
	}
	for _, id := range th.Pending {
		if !seen[id] {
			pending = append(pending, id)
		}
	}
	th.Pending = pending
}

// Reevaluate 接受 Trigger ID（string）或 ThreadRef。前者重排指定事件，后者重排对象仍待处理的失败。
// 返回是否有事件重新请求了研判。
func (e *Engine) Reevaluate(ctx context.Context, ref any) (bool, error) {
	var queued bool
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		now := e.cfg.Clock()
		var err error
		switch v := ref.(type) {
		case string:
			queued, err = retryOne(u, v, now)
		case ThreadRef:
			th, readErr := u.GetThread(ThreadKey(v))
			if readErr != nil || th == nil {
				return readErr
			}
			queued, err = retryUnavailable(u, th, now)
		default:
			return fmt.Errorf("重判引用应为 Trigger ID 或 ThreadRef，得到 %T", v)
		}
		return err
	})
	if err != nil {
		return false, fmt.Errorf("人工重判 %v: %w", ref, err)
	}
	return queued, nil
}

func retryOne(u Unit, id, now string) (bool, error) {
	tr, err := requireTrigger(u, id)
	if err != nil {
		return false, err
	}
	th, err := triggerThread(u, tr, now)
	if err != nil {
		return false, err
	}
	d, err := u.GetDisposition(id)
	if err != nil {
		return false, err
	}
	queued, err := requeueTrigger(u, th, id, d, now)
	if err != nil || !queued {
		return false, err
	}
	return true, u.PutThread(*th)
}

func retryUnavailable(u Unit, th *Thread, now string) (bool, error) {
	history, err := u.DecisionsByThread(th.Key)
	if err != nil {
		return false, err
	}
	// 从旧到新保留失败批次的顺序。每个事件只重试其最新且仍对人可见的失败。
	slices.SortFunc(history, func(a, b Decision) int { return cmp.Or(cmp.Compare(a.At, b.At), cmp.Compare(a.ID, b.ID)) })
	latest := map[string]string{}
	var ids []string
	for _, d := range history {
		for _, id := range d.TriggerIDs {
			if _, ok := latest[id]; !ok {
				ids = append(ids, id)
			}
			latest[id] = d.ID
		}
	}
	disps, err := u.GetDispositions(ids...)
	if err != nil {
		return false, err
	}
	var retry []string
	for _, d := range history {
		if d.Kind != Unavailable || d.Dismissed || internalRequeue(d.Reason) {
			continue
		}
		for _, id := range d.TriggerIDs {
			disp := disps[id]
			last := latest[id]
			// 处置指向实际最后一次提交，同毫秒完成的决策也能准确区分。
			if disp != nil && disp.DecisionID != "" {
				last = disp.DecisionID
			}
			if last != d.ID || slices.Contains(th.Pending, id) || slices.ContainsFunc(d.Reason, func(r Reason) bool { return r.Code == ReasonRouted && r.TriggerID == id }) {
				continue
			}
			retry = append(retry, id)
		}
	}
	if len(retry) == 0 {
		return false, nil
	}
	triggers, err := u.GetTriggers(retry...)
	if err != nil {
		return false, err
	}
	queued := false
	for _, id := range retry {
		if triggers[id] == nil {
			return false, &UserError{Code: CodeUnknownTrigger}
		}
		ok, err := requeueTrigger(u, th, id, disps[id], now)
		if err != nil {
			return false, err
		}
		queued = queued || ok
	}
	if !queued {
		return false, nil
	}
	return true, u.PutThread(*th)
}

func requeueTrigger(u Unit, th *Thread, id string, d *TriggerDisposition, now string) (bool, error) {
	if (d != nil && d.Recorded) || holding(th, id) {
		return false, nil
	}
	if !slices.Contains(th.Pending, id) {
		th.Pending = append(th.Pending, id)
	}
	if d == nil {
		d = &TriggerDisposition{At: now}
	}
	d.Requeued = true
	if err := u.PutDisposition(id, *d); err != nil {
		return false, err
	}
	return true, emitTrigger(u, "trigger.requeued", id, th.Key, now)
}
