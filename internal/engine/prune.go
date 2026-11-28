package engine

import (
	"context"
	"fmt"
	"slices"
)

// MinRetainedEvents 来自 06-runtime §7，保证补拉至少还能取到最近这些修订。
const MinRetainedEvents = 5000

// PruneResult 只计实际删除的行，WorkIDs 供提交后清理对应目录。
type PruneResult struct {
	Works, Decisions, Triggers, Dispositions, TriggerThreads, Threads, Events int
	WorkIDs                                                                   []string
}

// Prune 在同一事务中按三个严格上界清理，只有提交成功才交出 Work ID。
func (e *Engine) Prune(ctx context.Context, triggerBefore, workBefore, eventBefore string) (PruneResult, error) {
	for _, at := range []*string{&triggerBefore, &workBefore, &eventBefore} {
		v, err := IsoTime(*at)
		if err != nil {
			return PruneResult{}, fmt.Errorf("清理截止时间: %w", err)
		}
		*at = v
	}
	var result PruneResult
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		var err error
		result, err = u.Prune(triggerBefore, workBefore, eventBefore)
		return err
	})
	if err != nil {
		return PruneResult{}, fmt.Errorf("清理保留期外的记录: %w", err)
	}
	return result, nil
}

// PruneObject 使用集合与标识共同区分可同名的 Watch 对象。
type PruneObject struct{ Kind, ID string }

// PruneCandidates 只含过期候选及其直接引用方；候选之外的引用方都是保护根。
type PruneCandidates map[PruneObject][]PruneObject

// Expired 消耗候选表，返回本轮可以一起删除的对象。过期 Work 和它的 Decision 可以相互引用，
// 单看引用计数会永久保留这类环；从存活引用向下传播可以在一次事务内同时回收。
func (c PruneCandidates) Expired() map[string][]string {
	dependents := make(map[PruneObject][]PruneObject)
	var live []PruneObject
	for obj, refs := range c {
		root := false
		for _, ref := range refs {
			if _, candidate := c[ref]; candidate {
				dependents[ref] = append(dependents[ref], obj)
			} else {
				root = true
			}
		}
		if root {
			live = append(live, obj)
		}
	}
	for len(live) > 0 {
		obj := live[len(live)-1]
		live = live[:len(live)-1]
		if _, ok := c[obj]; ok {
			delete(c, obj)
			live = append(live, dependents[obj]...)
		}
	}
	c.keepReferencedThreads(dependents)
	expired := make(map[string][]string)
	for obj := range c {
		expired[obj.Kind] = append(expired[obj.Kind], obj.ID)
	}
	for _, ids := range expired {
		slices.Sort(ids)
	}
	return expired
}

// 关闭对象还须「无任何引用」。规格未授权拆开相互 correlated 的对象环；
// 只从已无引用的 Thread 开始移除，链可以清完，环和由环引用的对象继续保留。
func (c PruneCandidates) keepReferencedThreads(dependents map[PruneObject][]PruneObject) {
	incoming := map[PruneObject]int{}
	var free []PruneObject
	for obj, refs := range c {
		if obj.Kind != "thread" {
			continue
		}
		count := 0
		for _, ref := range refs {
			if ref.Kind == "thread" {
				count++
			}
		}
		if count == 0 {
			free = append(free, obj)
		} else {
			incoming[obj] = count
		}
	}
	for len(free) > 0 {
		th := free[len(free)-1]
		free = free[:len(free)-1]
		for _, obj := range dependents[th] {
			n := incoming[obj]
			if n == 1 {
				delete(incoming, obj)
				free = append(free, obj)
			} else if n > 1 {
				incoming[obj] = n - 1
			}
		}
	}
	for obj := range incoming {
		delete(c, obj)
	}
}
