package engine

import "slices"

// 无跟随时只有执行方的 ready 能证明完成；有自有对象时结果取最后一次关闭事实。
func followsDone(w *Work) *ThreadClose {
	if len(w.Follows) == 0 {
		if n := len(w.Assignments); n > 0 {
			if r := w.Assignments[n-1].Report; r != nil && r.State == Ready {
				return &ThreadClose{At: r.At, Outcome: string(Completed)}
			}
		}
		return nil
	}
	var last *ThreadClose
	for i := range w.Follows {
		f := &w.Follows[i]
		if !f.Own {
			continue
		}
		if f.Closed == nil {
			return nil
		}
		if last == nil || f.Closed.At >= last.At {
			last = f.Closed
		}
	}
	return last
}

func sameClose(a, b *ThreadClose) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// 在外部关闭事实之后才启动的派工可能仍在处理更新的要求，不能用旧关闭抢先结束。
func unfinished(w *Work, closedAt string) bool {
	// 人的消息已送达、会话还没取走（readAt 为 null）时结束，这条消息就悄悄丢了。
	// 这一条与派工的先后无关，所以放在派工判断之前。纯 context 跟随的副本不要求被读。
	if slices.ContainsFunc(w.Inputs, func(in WorkInput) bool {
		return in.Kind == "trigger" && in.Human && !in.Context && in.ReadAt == (ReadTime{Present: true})
	}) {
		return true
	}
	if len(w.Assignments) == 0 {
		return false
	}
	a := &w.Assignments[len(w.Assignments)-1]
	if a.CreatedAt <= closedAt {
		return false
	}
	r := a.Report
	if r == nil || r.State != Ready || len(w.Undelivered) > 0 {
		return true
	}
	return slices.ContainsFunc(w.Inputs, func(in WorkInput) bool { return in.Kind != "policy-changed" && in.At > r.At })
}

func propagateThreadStatus(u Unit, th *Thread, now string) error {
	// endWork 会删除订阅。固定收件人并复用 Thread，不能一边遍历原切片一边删除。
	ids := slices.Clone(th.Subscriptions)
	if len(ids) == 0 {
		return nil
	}
	works, err := u.GetWorks(ids...)
	if err != nil {
		return err
	}
	threads := map[string]*Thread{th.Key: th}
	for _, id := range ids {
		w := works[id]
		if w == nil || w.Ended != nil {
			continue
		}
		i := followIndex(w, th.Key)
		if i < 0 {
			continue
		}
		f := &w.Follows[i]
		if sameClose(th.Closed, f.Closed) {
			continue
		}
		f.Closed = th.Closed
		w.UpdatedAt = now
		if th.Closed != nil {
			if done := followsDone(w); done != nil && !unfinished(w, done.At) {
				at := done.At
				if n := len(w.Assignments); n > 0 && w.Assignments[n-1].CreatedAt > done.At {
					at = now
				}
				end := WorkEnd{By: "executor", At: at, Outcome: WorkOutcome(done.Outcome)}
				if err := endWork(u, w, end, threads, now); err != nil {
					return err
				}
				continue
			}
		}
		if err := u.PutWork(*w); err != nil {
			return err
		}
	}
	return nil
}
