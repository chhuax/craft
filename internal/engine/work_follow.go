package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// 工作命令只持有本事务内的一份聚合。执行方入口统一拒绝操作已经结束的工作。
func (e *Engine) withWork(ctx context.Context, action, id string, openOnly bool, fn func(Unit, *Work, string) error) (Work, error) {
	var w *Work
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		var err error
		w, err = u.GetWork(id)
		if err != nil {
			return err
		}
		if w == nil {
			return &UserError{Code: CodeUnknownWork}
		}
		if openOnly && w.Ended != nil {
			return &UserError{Code: CodeWorkEnded}
		}
		return fn(u, w, e.cfg.Clock())
	})
	if err != nil {
		return Work{}, fmt.Errorf("%s Work %q: %w", action, id, err)
	}
	return *w, nil
}

func followIndex(w *Work, key string) int {
	return slices.IndexFunc(w.Follows, func(f Follow) bool { return f.Thread == key })
}

// BindThread 登记执行方建的对象。自动来源撤回时仍能恢复手工指定的归属。
func (e *Engine) BindThread(ctx context.Context, workID string, ref ThreadRef, own bool) (Work, error) {
	return e.withWork(ctx, "绑定对象", workID, true, func(u Unit, w *Work, now string) error {
		th, err := u.GetThread(ThreadKey(ref))
		if err != nil {
			return err
		}
		th = threadOrNew(th, ref, now)
		if own {
			if e.cfg.Scope == nil {
				return errors.New("绑定自有对象需要 SourceScope")
			}
			if e.cfg.Scope.Closable(ref.Source(), ref.Kind()) {
				if err := checkOwnAvailable(u, []*Thread{th}, w.ID, nil); err != nil {
					return err
				}
			}
		}
		if err := subscribeWork(u, th, w.ID, now); err != nil {
			return err
		}
		i := followIndex(w, th.Key)
		if i < 0 {
			i = len(w.Follows)
			w.Follows = append(w.Follows, Follow{Thread: th.Key})
		}
		f := &w.Follows[i]
		// 再次手工绑定开始新的跟随；已经明确认领过的手工绑定不能被降级。
		f.Closed = nil
		f.Own = f.Own || own
		f.Manual = &ManualFollow{Own: own || (f.Manual != nil && f.Manual.Own)}
		w.UpdatedAt = now
		return u.PutWork(*w)
	})
}

// FollowForContext 只向工作分发只读副本。已有正常订阅时保留其认领资格。
func (e *Engine) FollowForContext(ctx context.Context, workID string, ref ThreadRef, reason string) (bool, error) {
	added := false
	_, err := e.withWork(ctx, "只读跟随", workID, true, func(u Unit, w *Work, now string) error {
		th, err := u.GetThread(ThreadKey(ref))
		if err != nil {
			return err
		}
		th = threadOrNew(th, ref, now)
		if slices.Contains(th.Subscriptions, w.ID) {
			return nil
		}
		th.Subscriptions = append(th.Subscriptions, w.ID)
		if !slices.Contains(th.Context, w.ID) {
			th.Context = append(th.Context, w.ID)
		}
		if err := u.PutThread(*th); err != nil {
			return err
		}
		added = true
		return subscriptionEvent(u, "thread.subscribed", th.Key, w.ID, "executor", reason, now)
	})
	return added && err == nil, err
}

func subscriptionEvent(u Unit, kind, key, id, by, reason, now string) error {
	data := map[string]any{"threadKey": key, "workId": id, "by": by}
	if reason != "" {
		data["reason"] = reason
	}
	return emitEvent(u, kind, now, map[string]string{"thread": key, "work": id}, data)
}

func removeSubscription(th *Thread, id string) {
	th.Subscriptions = slices.DeleteFunc(th.Subscriptions, func(s string) bool { return s == id })
	th.Context = slices.DeleteFunc(th.Context, func(s string) bool { return s == id })
}

// Unsubscribe 解除该对象上的全部订阅和 Follow；已退订的对象可安全重试。
func (e *Engine) Unsubscribe(ctx context.Context, key string) (bool, error) {
	changed := false
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		th, err := u.GetThread(key)
		if err != nil || th == nil {
			return err
		}
		ids := uniqueIDs(th.Subscriptions, th.Context)
		if len(ids) == 0 {
			return nil
		}
		rows, err := u.GetWorks(ids...)
		if err != nil {
			return err
		}
		now := e.cfg.Clock()
		for _, id := range ids {
			if w := rows[id]; w != nil {
				if i := followIndex(w, key); i >= 0 {
					w.Follows = slices.Delete(w.Follows, i, i+1)
					w.UpdatedAt = now
					if err := u.PutWork(*w); err != nil {
						return err
					}
				}
			}
			if err := subscriptionEvent(u, "thread.unsubscribed", key, id, "person", "", now); err != nil {
				return err
			}
		}
		th.Subscriptions = nil
		th.Context = nil
		changed = true
		return u.PutThread(*th)
	})
	if err != nil {
		return false, fmt.Errorf("退订 Thread %q: %w", key, err)
	}
	return changed, nil
}

func uniqueIDs(groups ...[]string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, group := range groups {
		for _, id := range group {
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
	}
	return ids
}
