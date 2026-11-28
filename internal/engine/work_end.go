package engine

import (
	"context"
	"fmt"
	"maps"
)

// StopWork 记录人的停止决定。重试保留第一次的结束事实和停止请求。
func (e *Engine) StopWork(ctx context.Context, workID, reason string) (Work, error) {
	return e.withWork(ctx, "停止", workID, false, func(u Unit, w *Work, now string) error {
		if w.Ended != nil {
			return nil
		}
		return endWork(u, w, WorkEnd{At: now, By: "human", Outcome: Stopped, Reason: reason}, map[string]*Thread{}, now)
	})
}

// 同一对象可能触发多个工作结束，共享已读取的 Thread，防止后一份旧副本覆盖前一次退订。
func endWork(u Unit, w *Work, end WorkEnd, threads map[string]*Thread, now string) error {
	if err := unsubscribeEnded(u, w, threads, end.By, end.Reason, now); err != nil {
		return err
	}
	if end.Reason == string(end.Outcome) {
		end.Reason = ""
	}
	w.Ended = &end
	w.UpdatedAt = now
	if err := withdrawQuestion(u, w.Attention); err != nil {
		return err
	}
	w.Attention = nil
	w.Delivery = nil
	w.Undelivered = nil
	w.DeliveryBatch = nil
	var running []string
	for i := range w.Assignments {
		a := &w.Assignments[i]
		if a.State == AssignmentRunning {
			running = append(running, a.ID)
		}
		if a.State != AssignmentEnded {
			a.State = AssignmentEnded
			a.EndedAt = end.At
		}
	}
	if err := u.PutWork(*w); err != nil {
		return err
	}
	data := map[string]any{"workId": w.ID, "outcome": end.Outcome, "by": end.By}
	if end.Reason != "" {
		data["reason"] = end.Reason
	}
	if err := emitEvent(u, "work.ended", now, map[string]string{"work": w.ID}, data); err != nil {
		return err
	}
	for _, id := range running {
		if err := enqueueStop(u, w, id); err != nil {
			return err
		}
	}
	return nil
}

func unsubscribeEnded(u Unit, w *Work, threads map[string]*Thread, by, reason, now string) error {
	keys, err := u.SubscribedThreadKeys(w.ID)
	if err != nil {
		return err
	}
	var missing []string
	for _, key := range keys {
		if threads[key] == nil {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		found, err := u.GetThreads(missing...)
		if err != nil {
			return err
		}
		maps.Copy(threads, found)
	}
	for _, key := range keys {
		th := threads[key]
		if th == nil {
			return fmt.Errorf("Work %q 订阅的 Thread %q 缺失", w.ID, key)
		}
		removeSubscription(th, w.ID)
		if err := u.PutThread(*th); err != nil {
			return err
		}
		if err := subscriptionEvent(u, "thread.unsubscribed", key, w.ID, by, reason, now); err != nil {
			return err
		}
	}
	return nil
}

func enqueueStop(u Unit, w *Work, id string) error {
	// 没有登记会话就没有可停的东西。这里不能报错：数据一旦出现「运行中却没有会话」的不一致，
	// 报错会让人永远停不掉这个 Work，比漏发一次停止请求糟糕得多。
	if w.Execution == nil || w.Execution.Session == nil || w.Execution.Session.ID == "" || w.Execution.Session.Host == "" {
		return nil
	}
	// 即使下一次派工改变了执行位置，停止请求也必须指向旧会话的快照。
	return enqueueEffect(u, "stop", id, map[string]any{"workId": w.ID, "assignmentId": id, "session": w.Execution.Session})
}

// RetryWork 重试投递故障或替换受阻、失败的派工；后者在新 launch 前登记旧轮的 stop。
func (e *Engine) RetryWork(ctx context.Context, workID string) (Work, error) {
	return e.withWork(ctx, "重试", workID, false, func(u Unit, w *Work, now string) error {
		if w.Ended != nil || len(w.Assignments) == 0 {
			return &UserError{Code: CodeNotRetryable}
		}
		old := &w.Assignments[len(w.Assignments)-1]
		if w.Delivery != nil {
			return retryDelivery(u, w, old, now)
		}
		if old.Report == nil || (old.Report.State != Blocked && old.Report.State != Failed) || answeredSince(w, old.Report.At) {
			return &UserError{Code: CodeNotRetryable}
		}
		a, err := replaceAssignment(u, w, old, "retried", now)
		if err != nil {
			return err
		}
		if err := u.PutWork(*w); err != nil {
			return err
		}
		return enqueueLaunch(u, w.ID, a.ID)
	})
}

func retryDelivery(u Unit, w *Work, old *Assignment, now string) error {
	if old.State != AssignmentPending && old.State != AssignmentRunning {
		return &UserError{Code: CodeNotRetryable}
	}
	w.Delivery = nil
	w.UpdatedAt = now
	if err := u.PutWork(*w); err != nil {
		return err
	}
	if old.State == AssignmentRunning {
		return enqueueEffect(u, "delivery", w.ID, map[string]string{"workId": w.ID})
	}
	return enqueueLaunch(u, w.ID, old.ID)
}

func requeueUnread(w *Work, id string) {
	// readAt 缺失是旧数据，只有明确未读、且送给被替换派工的输入可以重新投递。
	queued := make(map[string]bool, len(w.Undelivered))
	for _, id := range w.Undelivered {
		queued[id] = true
	}
	var unread []string
	for _, in := range w.Inputs {
		if in.Kind == "trigger" && in.DeliveredTo == id && in.ReadAt == (ReadTime{Present: true}) && !queued[in.ID] {
			unread = append(unread, in.ID)
			queued[in.ID] = true
		}
	}
	w.Undelivered = append(unread, w.Undelivered...)
}

func enqueueLaunch(u Unit, workID, assignmentID string) error {
	return enqueueEffect(u, "launch", assignmentID, map[string]string{"assignmentId": assignmentID, "workId": workID})
}
