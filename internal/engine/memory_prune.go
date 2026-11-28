package engine

import "slices"

func (u *memoryUnit) pruneCandidates(kind, before string, c PruneCandidates) error {
	prefix := kind + "\x00"
	for _, entry := range u.pruneDates.between(prefix, prefix+before) {
		obj := PruneObject{kind, string(entry.data)}
		if kind == "trigger" {
			d, err := u.GetDisposition(obj.ID)
			if err != nil {
				return err
			}
			if d != nil && d.At >= before {
				continue
			}
		}
		var refs []PruneObject
		idx := u.pruneRefs.prefixTree(kind + "\x00" + linkPrefix(obj.ID))
		for _, ref := range idx.between("", "") {
			refs = append(refs, PruneObject{ref.lookup, string(ref.data)})
		}
		if kind == "thread" {
			for _, id := range u.threads.get(obj.ID).subscribers {
				refs = append(refs, PruneObject{"work", id})
			}
			ids, err := u.TriggerIDs(obj.ID)
			if err != nil {
				return err
			}
			for _, id := range ids {
				refs = append(refs, PruneObject{"trigger", id})
			}
		}
		c[obj] = refs
	}
	return nil
}

// Prune 将主文档和全部反查索引一起写入当前快照，回滚时一起丢弃。
func (u *memoryUnit) Prune(triggerBefore, workBefore, eventBefore string) (PruneResult, error) {
	c := PruneCandidates{}
	// Thread 与 Work、Decision 同属长寿命记录，已关闭对象使用较长的 workBefore。
	for kind, before := range map[string]string{"work": workBefore, "decision": workBefore, "thread": workBefore, "trigger": triggerBefore} {
		if err := u.pruneCandidates(kind, before, c); err != nil {
			return PruneResult{}, err
		}
	}
	expired := c.Expired()
	result := PruneResult{WorkIDs: expired["work"], Works: len(expired["work"]), Decisions: len(expired["decision"]), Threads: len(expired["thread"]), Triggers: len(expired["trigger"])}
	if err := u.unsubscribeExpired(result.WorkIDs); err != nil {
		return PruneResult{}, err
	}
	for _, id := range result.WorkIDs {
		u.removePruned(&u.works, "work", id)
	}
	for _, id := range expired["decision"] {
		d, err := u.GetDecision(id)
		if err != nil {
			return PruneResult{}, err
		}
		u.indexDecision(d, nil)
		u.removePruned(&u.decisions, "decision", id)
	}
	for _, id := range expired["trigger"] {
		if err := u.pruneTrigger(id, &result); err != nil {
			return PruneResult{}, err
		}
	}
	for _, key := range expired["thread"] {
		u.removePruned(&u.threads, "thread", key)
	}
	result.Events = u.pruneEvents(eventBefore)
	return result, nil
}

func (u *memoryUnit) removePruned(tree **indexNode, kind, id string) {
	u.retention(kind, id, (*tree).get(id), nil, retentionIndex{})
	*tree = (*tree).set(u.mark, id, nil)
}

func (u *memoryUnit) unsubscribeExpired(ids []string) error {
	remove := make(map[string]bool, len(ids))
	threads := make(map[string]bool)
	for _, id := range ids {
		remove[id] = true
		keys, err := u.SubscribedThreadKeys(id)
		if err != nil {
			return err
		}
		for _, key := range keys {
			threads[key] = true
		}
	}
	for key := range threads {
		th, err := u.GetThread(key)
		if err != nil {
			return err
		}
		gone := func(id string) bool { return remove[id] }
		th.Subscriptions = slices.DeleteFunc(th.Subscriptions, gone)
		th.Context = slices.DeleteFunc(th.Context, gone)
		if err := u.PutThread(*th); err != nil {
			return err
		}
	}
	return nil
}

func (u *memoryUnit) pruneTrigger(id string, result *PruneResult) error {
	keys, err := u.ThreadKeys(id)
	if err != nil {
		return err
	}
	for _, key := range keys {
		u.threadsByTrigger = u.threadsByTrigger.set(u.mark, linkPrefix(id)+key, nil)
		u.triggersByThread = u.triggersByThread.set(u.mark, linkPrefix(key)+id, nil)
	}
	result.TriggerThreads += len(keys)
	if u.dispositions.get(id) != nil {
		u.removePruned(&u.dispositions, "disposition", id)
		result.Dispositions++
	}
	u.removePruned(&u.triggers, "trigger", id)
	return nil
}

func (u *memoryUnit) pruneEvents(before string) int {
	if u.rev <= MinRetainedEvents {
		return 0
	}
	last := revisionKey(u.rev - MinRetainedEvents)
	var ids []string
	for _, entry := range u.pruneDates.between("event\x00", "event\x00"+before) {
		if id := string(entry.data); id <= last {
			ids = append(ids, id)
		}
	}
	// 同一个保存点可能已修改过这些索引节点，遍历结束后再逐键删除。
	for _, id := range ids {
		u.removePruned(&u.events, "event", id)
	}
	return len(ids)
}
