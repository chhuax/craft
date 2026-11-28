package engine

import (
	"context"
	"fmt"
)

func sessionMatches(w *Work, session *Session) bool {
	return w != nil && session != nil && session.Host != "" && session.ID != "" && w.Execution != nil && w.Execution.Session != nil && *w.Execution.Session == *session
}

func currentAssignment(w *Work) *Assignment {
	if w == nil || w.Ended != nil || len(w.Assignments) == 0 {
		return nil
	}
	a := &w.Assignments[len(w.Assignments)-1]
	if a.State != AssignmentRunning && a.State != AssignmentPending {
		return nil
	}
	return a
}

// SetExecution 登记最新派工的目录和会话，保留目标快照。传空会话清除已失联的登记。
// 返回 false 表示代次失效或已有另一个会话，创建方须停止未被接纳的会话。
func (e *Engine) SetExecution(ctx context.Context, workID, assignmentID, directory string, session *Session, bootstrapped bool) (bool, error) {
	var accepted bool
	_, err := e.withWork(ctx, "登记执行位置", workID, false, func(u Unit, w *Work, now string) error {
		a := currentAssignment(w)
		if a == nil || a.ID != assignmentID {
			return nil
		}
		if w.Execution == nil {
			w.Execution = &Execution{}
		}
		x := w.Execution
		if session != nil && (session.Host == "" || session.ID == "") {
			return fmt.Errorf("会话缺少宿主或标识")
		}
		if session != nil && x.Session != nil && *x.Session != *session {
			return nil
		}
		accepted = true
		if session != nil && x.Session != nil && x.Bootstrapped != nil && *x.Bootstrapped {
			bootstrapped = true
		}
		if directory == x.Directory && x.Assignment == a.ID && (session == nil && x.Session == nil || sessionMatches(w, session)) && x.Bootstrapped != nil && *x.Bootstrapped == bootstrapped {
			return nil
		}
		x.Directory, x.Session, x.Assignment, x.Bootstrapped = directory, session, a.ID, &bootstrapped
		w.UpdatedAt = now
		return u.PutWork(*w)
	})
	return accepted && err == nil, err
}

// AssignmentStarted 确认最新派工的首条送达。重复回执不覆盖轮次起点，也不重复唤醒。
func (e *Engine) AssignmentStarted(ctx context.Context, workID, assignmentID, at string) error {
	_, err := e.withWork(ctx, "确认启动", workID, false, func(u Unit, w *Work, now string) error {
		a := currentAssignment(w)
		if a == nil || a.ID != assignmentID || a.State == AssignmentRunning && a.DeliveredAt != "" {
			return nil
		}
		start, err := deliveryTime(at, now)
		if err != nil {
			return err
		}
		if w.Execution != nil && w.Execution.Session != nil {
			w.Execution.Bootstrapped = new(true)
			w.Execution.Assignment = a.ID
		}
		a.State = AssignmentRunning
		if a.StartedAt == "" {
			a.StartedAt = now
		}
		a.DeliveredAt = start
		w.UpdatedAt = now
		w.Delivery = nil
		if err := u.PutWork(*w); err != nil {
			return err
		}
		if err := assignmentEvent(u, w, a, "assignment.started", now, nil); err != nil {
			return err
		}
		return wake(u, w)
	})
	return err
}

// TurnEnded 仅确认当前运行派工的轮末。监视方负责按会话识别、在送达回写后重放早到的 Stop。
func (e *Engine) TurnEnded(ctx context.Context, workID, assignmentID string) error {
	_, err := e.withWork(ctx, "处理轮末", workID, false, func(u Unit, w *Work, now string) error {
		a := currentAssignment(w)
		if a == nil || a.ID != assignmentID || a.State != AssignmentRunning {
			return nil
		}
		a.IdleAt = now
		w.UpdatedAt = now
		return finishOrSave(u, w, now, "")
	})
	return err
}

func finishOrSave(u Unit, w *Work, now string, reported ReportState) error {
	done := followsDone(w)
	if reported == ReportDropped {
		outcome := Dropped
		if done != nil {
			outcome = WorkOutcome(done.Outcome)
		}
		return endWork(u, w, WorkEnd{At: now, By: "executor", Outcome: outcome}, map[string]*Thread{}, now)
	}
	if done == nil || unfinished(w, done.At) {
		return u.PutWork(*w)
	}
	at := done.At
	// 新派工后来完成，或当前刚刚报 ready 时，结束时刻取本轮的确认时间。
	if reported == Ready || w.Assignments[len(w.Assignments)-1].CreatedAt > at {
		at = now
	}
	return endWork(u, w, WorkEnd{At: at, By: "executor", Outcome: WorkOutcome(done.Outcome)}, map[string]*Thread{}, now)
}

// DeliveryFailed 把失败或结果未知的首条、增量交给人核对；原批次和所有待投输入保留。
func (e *Engine) DeliveryFailed(ctx context.Context, workID, assignmentID, reason string) error {
	_, err := e.withWork(ctx, "记录投递失败", workID, false, func(u Unit, w *Work, now string) error {
		a := currentAssignment(w)
		if a == nil || a.ID != assignmentID {
			return nil
		}
		w.Delivery = &DeliveryFailure{At: now, Error: reason}
		w.UpdatedAt = now
		return u.PutWork(*w)
	})
	return err
}

// AssignmentInterrupted 在宿主会话丢失时恢复未完成的派工；已排队的投递自行重建会话。
func (e *Engine) AssignmentInterrupted(ctx context.Context, workID, assignmentID string) error {
	_, err := e.withWork(ctx, "处理中断", workID, false, func(u Unit, w *Work, now string) error {
		a := currentAssignment(w)
		if a == nil || a.ID != assignmentID || a.State != AssignmentRunning || w.Delivery != nil || w.Mandate.Basis.Via == ViaHumanDirect {
			return nil
		}
		queued, err := u.HasEffect("delivery", workID)
		if err != nil || queued {
			return err
		}
		if r := a.Report; r != nil && r.At >= a.DeliveredAt {
			switch r.State {
			case Waiting, Ready, Blocked, Failed:
				return wake(u, w)
			}
		}
		next, err := replaceAssignment(u, w, a, "hostRestarted", now)
		if err != nil {
			return err
		}
		note := "宿主在工作进行中重启。继续前请检查当前目录、版本记录和外部系统状态，避免重复已完成的对外操作。"
		in := WorkInput{ID: Key("input", w.ID, "restart", next.ID), Kind: "text", Text: &note, At: now}
		w.Inputs = append(w.Inputs, in)
		w.Undelivered = append(w.Undelivered, in.ID)
		if err := u.PutWork(*w); err != nil {
			return err
		}
		if w.Execution == nil || w.Execution.Target == nil {
			return nil
		}
		return enqueueLaunch(u, w.ID, next.ID)
	})
	return err
}

func assignmentEvent(u Unit, w *Work, a *Assignment, kind, now string, data map[string]any) error {
	if data == nil {
		data = make(map[string]any)
	}
	data["workId"], data["assignmentId"] = w.ID, a.ID
	return emitEvent(u, kind, now, map[string]string{"work": w.ID, "assignment": a.ID}, data)
}

// 替换派工时应先登记旧会话的停止，再由调用方保存新一代并排 launch。
func replaceAssignment(u Unit, w *Work, old *Assignment, reason, now string) (Assignment, error) {
	old.State = AssignmentEnded
	old.EndedAt = now
	old.EndedReason = reason
	if err := assignmentEvent(u, w, old, "assignment.ended", now, map[string]any{"reason": reason}); err != nil {
		return Assignment{}, err
	}
	// 中断回调已经确认会话消失；仅原地重试需要先停旧轮。
	if reason == "retried" {
		if err := enqueueStop(u, w, old.ID); err != nil {
			return Assignment{}, err
		}
	}
	requeueUnread(w, old.ID)
	w.DeliveryBatch = nil
	a := Assignment{ID: Key("assignment", w.ID, fmt.Sprint(len(w.Assignments))), CreatedAt: now, State: AssignmentPending}
	w.Assignments = append(w.Assignments, a)
	w.UpdatedAt = now
	return a, nil
}
