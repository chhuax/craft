package engine

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"fmt"
)

func threadOrNew(th *Thread, ref ThreadRef, now string) *Thread {
	if th != nil {
		return th
	}
	return &Thread{Key: ThreadKey(ref), Ref: ref, CreatedAt: now}
}

// ThreadClosed 记录关闭事实，并在同一事务内更新订阅工作的 Follow 与结束状态。
func (e *Engine) ThreadClosed(ctx context.Context, ref ThreadRef, outcome, at, detail string) error {
	return e.setThreadStatus(ctx, ref, at, &ThreadClose{Outcome: outcome, Detail: detail})
}

// ThreadReopened 清除在办工作的关闭标记，并保留重开时间以抵御迟到的关闭。
func (e *Engine) ThreadReopened(ctx context.Context, ref ThreadRef, at string) error {
	return e.setThreadStatus(ctx, ref, at, nil)
}

func (e *Engine) setThreadStatus(ctx context.Context, ref ThreadRef, at string, closed *ThreadClose) error {
	key := ThreadKey(ref)
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		if closed != nil && closed.Outcome != "done" && closed.Outcome != "closed" {
			return fmt.Errorf("未知关闭结果 %q", closed.Outcome)
		}
		when, err := IsoTime(at)
		if err != nil {
			return fmt.Errorf("规范化状态时间: %w", err)
		}
		th, err := u.GetThread(key)
		if err != nil {
			return err
		}
		now := e.cfg.Clock()
		th = threadOrNew(th, ref, now)
		last := th.ReopenedAt
		if th.Closed != nil {
			last = max(last, th.Closed.At)
		}
		if when < last {
			return nil
		}
		kind := "thread.reopened"
		if closed == nil {
			// 同毫秒无法区分先后，以关闭为准，使乱序重放也得到相同结果。
			if when == last {
				return nil
			}
			th.ReopenedAt = when
		} else {
			closed.At = when
			if sameClose(th.Closed, closed) {
				// 同一事实无需再次广播，但手工重绑可能发生在两次相同通知之间。
				return propagateThreadStatus(u, th, now)
			}
			kind = "thread.closed"
		}
		th.Closed = closed
		if err := u.PutThread(*th); err != nil {
			return err
		}
		if err := emitThread(u, kind, key, now); err != nil {
			return err
		}
		return propagateThreadStatus(u, th, now)
	})
	if err != nil {
		return fmt.Errorf("更新 Thread %q 状态: %w", key, err)
	}
	return nil
}

func emitThread(u Unit, kind, key, now string) error {
	raw, err := jsontext.AppendQuote(nil, key)
	if err != nil {
		return fmt.Errorf("编码对象键: %w", err)
	}
	return u.Emit(Event{Type: kind, At: now, Ref: map[string]string{"thread": key}, Data: map[string]json.RawMessage{"threadKey": raw}})
}
