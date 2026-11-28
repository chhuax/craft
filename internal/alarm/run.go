package alarm

import (
	"context"
	"fmt"
	"time"
)

// admit 在锁内决定是否运行，跳过的请求不创建 goroutine，也不会留下待处理项。
func (s *Service) admit(e *entry, reason string, when time.Time) bool {
	group := e.spec.Group
	limit := s.limits[group]
	if limit == 0 {
		limit = 1
	}
	if e.cancel != nil || s.groups[group] >= limit || s.active >= s.cfg.GlobalLimit {
		e.snap.LastResult, e.snap.LastError = "skipped", ""
		return false
	}
	ctx, cancel := context.WithCancelCause(s.ctx)
	e.cancel = cancel
	s.groups[group]++
	s.active++
	req := RunRequest{ID: e.spec.ID, Reason: reason, ScheduledAt: when}
	go s.execute(ctx, e, req, s.cfg.Clock.Now())
	return true
}

func (s *Service) execute(ctx context.Context, e *entry, req RunRequest, start time.Time) {
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		e.cancel(nil)
		e.cancel = nil
		if e.removed {
			delete(s.alarms, e.spec.ID)
		}
		group := e.spec.Group
		s.groups[group]--
		if s.groups[group] == 0 {
			delete(s.groups, group)
		}
		s.active--
		s.signal()
	}()
	if e.spec.Timeout == 0 {
		err := callRun(ctx, e.spec, req)
		s.record(e, err, s.cfg.Clock.Now())
		return
	}

	// 有超时时单独等 Run 的实际返回，这样不合作的回调也会准时记失败，但不会提前释放名额。
	// 等待者与回调共用一个名额，goroutine 总数受全局上限约束。
	type result struct {
		err error
		at  time.Time
	}
	clock := s.cfg.Clock
	deadline := start.Add(e.spec.Timeout)
	remaining := deadline.Sub(clock.Now())
	if remaining <= 0 {
		e.cancel(context.DeadlineExceeded)
		s.record(e, context.DeadlineExceeded, deadline)
		return
	}
	timer := clock.NewTimer(remaining)
	defer timer.Stop()
	done := make(chan result, 1)
	go func() {
		err := callRun(ctx, e.spec, req)
		done <- result{err, clock.Now()}
	}()
	var res result
	select {
	case res = <-done:
	case <-timer.C:
		// 两者都就绪时，以回调返回时刻判断是否超时，避免 select 的随机顺序改变结果。
		select {
		case res = <-done:
		default:
			e.cancel(context.DeadlineExceeded)
			s.record(e, context.DeadlineExceeded, deadline)
			<-done
			return
		}
	}
	if !res.at.Before(deadline) {
		e.cancel(context.DeadlineExceeded)
		res = result{context.DeadlineExceeded, deadline}
	}
	s.record(e, res.err, res.at)
}

func callRun(ctx context.Context, spec AlarmSpec, req RunRequest) (err error) {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	// Run 属于调用方；将 panic 归入这一次的失败，才能继续调度其他闹钟并归还名额。
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("闹钟 %q 的 Run panic: %v", req.ID, v)
		}
	}()
	return spec.Run(ctx, req)
}

func (s *Service) record(e *entry, err error, at time.Time) {
	s.mu.Lock()
	if e.removed {
		s.mu.Unlock()
		return
	}
	n := e.snap.ConsecutiveFailures
	threshold := s.cfg.FailureThreshold
	periodic := e.enabled && !s.stopping()
	if err == nil {
		e.snap.LastResult, e.snap.LastError = "success", ""
		e.snap.ConsecutiveFailures = 0
		if n > 0 && periodic {
			e.nominal = time.Time{}
			s.planRegular(e, at)
		}
	} else {
		e.snap.LastResult, e.snap.LastError = "failed", err.Error()
		e.snap.ConsecutiveFailures++
		if periodic {
			delays := [...]time.Duration{2 * time.Second, 10 * time.Second, time.Minute, 5 * time.Minute}
			e.snap.Next = at.Add(delays[min(n, len(delays)-1)])
			e.nominal = time.Time{}
		}
	}
	bad := e.snap.ConsecutiveFailures >= threshold
	onHealth := s.cfg.OnHealth
	s.signal()
	s.mu.Unlock()
	// 外部通知可能读取快照；在锁外通知，名额留到通知返回以维持同一 ID 的通知顺序。
	if onHealth != nil && bad != (n >= threshold) {
		onHealth(e.spec.ID, bad)
	}
}
