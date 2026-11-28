package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
)

// ReceiveResult 是事件提交后的去向；命令失败时返回空值。
type ReceiveResult string

// Stored 已进入分发或待研判；Duplicate 是重放；Filtered 与 Recorded 区分过滤及仅记动态。
const (
	Stored    ReceiveResult = "stored"
	Duplicate ReceiveResult = "duplicate"
	Filtered  ReceiveResult = "filtered"
	Recorded  ReceiveResult = "recorded"
)

// Receive 接收 SourceContext 已校验的来源事件，调用方可安全重放同一 ID。
// 内置的人对 Work 的回答由回答命令直投，不能作为来源事件分发。
func (e *Engine) Receive(ctx context.Context, tr Trigger) (ReceiveResult, error) {
	var result ReceiveResult
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		var err error
		result, err = e.ReceiveIn(u, tr)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("接收 Trigger %q: %w", tr.ID, err)
	}
	return result, nil
}

// ReceiveIn 在调用方的写事务内接收事件。调用方须将错误传回事务，提交后结果才生效。
func (e *Engine) ReceiveIn(u Unit, tr Trigger) (ReceiveResult, error) {
	policy := e.cfg.Policy()
	at, err := IsoTime(tr.At)
	if err != nil {
		return "", err
	}
	tr.At = at
	if len(tr.Threads) == 0 {
		return "", errors.New("Trigger 至少需要一个 Thread")
	}
	old, err := u.GetTrigger(tr.ID)
	if err != nil {
		return "", err
	}
	if old != nil {
		return Duplicate, nil
	}
	if tr.Source == "craft" && tr.Event == "person.answer" {
		return "", errors.New("人的回答须由 Answer 定向投递")
	}
	if e.cfg.Scope == nil {
		return "", errors.New("接收 Trigger 需要 SourceScope")
	}
	scope := e.cfg.Scope.Scope(tr.Source, tr.Event, tr.Meta)
	now := e.cfg.Clock()
	if err := u.PutTrigger(tr); err != nil {
		return "", err
	}
	keys := triggerThreadKeys(tr.Threads)
	if !scope.Listened {
		d := TriggerDisposition{At: now, Filtered: &Reason{Code: cmp.Or(scope.Reason, ReasonSourceOff)}}
		if err := u.PutDisposition(tr.ID, d); err != nil {
			return "", err
		}
		return Filtered, emitReceived(u, tr.ID, keys, now)
	}
	return routeTrigger(u, tr, keys, policy, e.cfg.Scope, now)
}

func triggerThreadKeys(refs []ThreadRef) []string {
	keys := make([]string, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		key := ThreadKey(ref)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	return keys
}

func routeTrigger(u Unit, tr Trigger, keys []string, p Policy, scope SourceScope, now string) (ReceiveResult, error) {
	threads, err := u.GetThreads(keys...)
	if err != nil {
		return "", err
	}
	// 批读省略缺失行；合法适配器可用 nil 表示整批均未命中。
	if threads == nil {
		threads = make(map[string]*Thread, len(keys))
	}
	for _, ref := range tr.Threads {
		key := ThreadKey(ref)
		threads[key] = threadOrNew(threads[key], ref, now)
	}
	if err := linkTriggerThreads(u, tr, keys, threads); err != nil {
		return "", err
	}
	d := TriggerDisposition{At: now}
	works, skipped, err := fanoutTrigger(u, tr, keys, threads, p, scope, &d)
	if err != nil {
		return "", err
	}
	def, _ := p.Event(tr.Source, tr.Event)
	result := routeUnclaimed(&d, def, threads[keys[0]], tr.ID, skipped)
	for _, key := range keys {
		if err := u.PutThread(*threads[key]); err != nil {
			return "", err
		}
	}
	if err := u.PutDisposition(tr.ID, d); err != nil {
		return "", err
	}
	for _, id := range d.Fanout {
		if err := wake(u, works[id]); err != nil {
			return "", err
		}
	}
	return result, emitReceived(u, tr.ID, keys, now)
}

func routeUnclaimed(d *TriggerDisposition, def EventDef, primary *Thread, id, skipped string) ReceiveResult {
	if !def.FollowupOnly {
		// 纯上下文订阅收到副本，但不能替这条事件认领工作。
		if len(d.Fanout) == len(d.Context) {
			primary.Pending = append(primary.Pending, id)
		}
		return Stored
	}
	if len(d.Fanout) != 0 {
		return Stored
	}
	if def.OnUnmatched == "record" {
		d.Recorded = true
		return Recorded
	}
	d.Filtered = &Reason{Code: cmp.Or(skipped, ReasonFollowupOnly)}
	return Filtered
}

func linkTriggerThreads(u Unit, tr Trigger, keys []string, threads map[string]*Thread) error {
	for _, key := range keys {
		th := threads[key]
		th.LastAt = max(th.LastAt, tr.At)
		if len(keys) > 1 {
			known := make(map[string]bool, len(th.Correlated))
			for _, other := range th.Correlated {
				known[other] = true
			}
			for _, other := range keys {
				if other != key && !known[other] {
					th.Correlated = append(th.Correlated, other)
				}
			}
		}
		if err := u.LinkTrigger(tr.ID, key); err != nil {
			return err
		}
	}
	return nil
}
