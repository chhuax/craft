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

// ThreadClosed 记录对象本身的关闭事实，较早的状态通知不覆盖已知事实。
func (e *Engine) ThreadClosed(ctx context.Context, ref ThreadRef, outcome, at, detail string) error {
	return e.setThreadStatus(ctx, ref, at, &ThreadClose{Outcome: outcome, Detail: detail})
}

// ThreadReopened 保留重开的水位，即使该对象此前从未关闭也能抵御迟到的关闭。
func (e *Engine) ThreadReopened(ctx context.Context, ref ThreadRef, at string) error {
	return e.setThreadStatus(ctx, ref, at, nil)
}

func (e *Engine) setThreadStatus(ctx context.Context, ref ThreadRef, at string, closed *ThreadClose) error {
	key := ThreadKey(ref)
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
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
			if th.Closed == nil && th.ReopenedAt == when {
				return nil
			}
			th.ReopenedAt = when
		} else {
			closed.At = when
			if th.Closed != nil && *th.Closed == *closed {
				return nil
			}
			kind = "thread.closed"
		}
		th.Closed = closed
		if err := u.PutThread(*th); err != nil {
			return err
		}
		// Works 生命周期应在此处、同一事务内传播已接受的事实，较早事实不能触及 Follow。
		return emitThread(u, kind, key, now)
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
