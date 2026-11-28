package engine

import (
	"encoding/binary"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
)

// 文档在 Put 时只编码一次；索引共享不可变行。Get 解码为独占对象，
// 同时让扩展 JSON 与 readAt 三态走和持久化适配器一致的模型边界。
func (u *memoryUnit) put(tree **indexNode, key string, v any) error {
	raw, err := jsonv2.Marshal(v)
	if err != nil {
		return fmt.Errorf("编码 %T[%q]: %w", v, key, err)
	}
	*tree = (*tree).set(u.mark, key, &memoryRow{data: raw})
	return nil
}

func (u *memoryUnit) putIndexed(tree, index **indexNode, key string, v any, active bool, lookup string) error {
	raw, err := jsonv2.Marshal(v)
	if err != nil {
		return fmt.Errorf("编码 %T[%q]: %w", v, key, err)
	}
	row := &memoryRow{data: raw, lookup: lookup}
	if old := (*tree).get(key); old != nil {
		row.order = old.order
	} else {
		u.insertions++
		row.order = string(binary.BigEndian.AppendUint64(nil, u.insertions))
	}
	*tree = (*tree).set(u.mark, key, row)
	if active {
		*index = (*index).set(u.mark, row.order, row)
	} else if (*index).get(row.order) != nil {
		*index = (*index).set(u.mark, row.order, nil)
	}
	return nil
}

func (u *memoryUnit) PutTrigger(v Trigger) error {
	// 同一事件可重放，第一次接收的外部事实保持不变。
	if u.triggers.get(v.ID) != nil {
		return nil
	}
	if err := u.put(&u.triggers, v.ID, &v); err != nil {
		return err
	}
	p := retentionIndex{at: v.At}
	for _, ref := range v.Threads {
		p.add("thread", ThreadKey(ref))
	}
	u.retention("trigger", v.ID, nil, u.triggers.get(v.ID), p)
	if len(v.Threads) > 0 {
		key := ThreadKey(v.Threads[0])
		u.triggers.get(v.ID).lookup = key
		// 导入时允许先写工作后写起源事件，此时才知道反查所需的主对象键。
		pending := "pending" + linkPrefix(v.ID)
		var waiting []*memoryRow
		for row := range u.openOrigins.prefixTree(pending).reverseRows() {
			waiting = append(waiting, row)
		}
		for _, row := range waiting {
			u.openOrigins = u.openOrigins.set(u.mark, pending+row.order, nil)
			u.openOrigins = u.openOrigins.set(u.mark, "thread"+linkPrefix(key)+row.order, row)
		}
	}
	return nil
}

func (u *memoryUnit) PutDisposition(id string, v TriggerDisposition) error {
	old := u.dispositions.get(id)
	if err := u.put(&u.dispositions, id, &v); err != nil {
		return err
	}
	u.retention("disposition", id, old, u.dispositions.get(id), dispositionRetention(&v))
	return nil
}

func (u *memoryUnit) PutThread(v Thread) error {
	eval := ""
	if v.Evaluating != nil {
		eval = v.Evaluating.ID
	}
	old := u.threads.get(v.Key)
	if err := u.putIndexed(&u.threads, &u.active, v.Key, &v, v.Closed == nil && v.Evaluating == nil && len(v.Pending) > 0, eval); err != nil {
		return err
	}
	row := u.threads.get(v.Key)
	u.retention("thread", v.Key, old, row, threadRetention(&v))
	u.indexSubscriptions(v, old, row)
	if old != nil && old.lookup != "" && old.lookup != eval {
		u.byEval = u.byEval.set(u.mark, old.lookup, nil)
	}
	if eval != "" {
		u.byEval = u.byEval.set(u.mark, eval, row)
	}
	return nil
}

func (u *memoryUnit) indexSubscriptions(th Thread, old, row *memoryRow) {
	// 某些旧数据退订时只清了 subscriptions，结束 Work 仍须清除孤立的 context。
	ids := append(slices.Clone(th.Subscriptions), th.Context...)
	slices.Sort(ids)
	row.subscribers = slices.Compact(ids)
	var previous []string
	if old != nil {
		previous = old.subscribers
	}
	for _, id := range previous {
		if _, keep := slices.BinarySearch(row.subscribers, id); !keep {
			u.subscribedThreads = u.subscribedThreads.set(u.mark, linkPrefix(id)+th.Key, nil)
		}
	}
	for _, id := range row.subscribers {
		if _, exists := slices.BinarySearch(previous, id); !exists {
			u.subscribedThreads = u.subscribedThreads.set(u.mark, linkPrefix(id)+th.Key, &memoryRow{data: []byte(th.Key)})
		}
	}
}

func (r *memoryView) SubscribedThreadKeys(workID string) ([]string, error) {
	return linkedKeys(r.subscribedThreads, workID)
}

func (u *memoryUnit) PutWork(v Work) error {
	key := ""
	if v.Ended == nil {
		key = v.CreatedAt + "\x00"
	}
	inputs, err := indexInputs(v.Inputs, v.Undelivered)
	if err != nil {
		return err
	}
	p := workRetention(&v)
	v.Inputs = nil
	v.Undelivered = nil
	old := u.works.get(v.ID)
	if err := u.putIndexed(&u.works, &u.open, v.ID, &v, v.Ended == nil, key); err != nil {
		return err
	}
	if old != nil && old.lookup != "" && old.lookup != key {
		u.recentOpen = u.recentOpen.set(u.mark, old.lookup+old.order, nil)
	}
	if key != "" && (old == nil || old.lookup != key) {
		u.recentOpen = u.recentOpen.set(u.mark, key+u.works.get(v.ID).order, &memoryRow{data: []byte(v.ID)})
	}
	row := u.works.get(v.ID)
	u.retention("work", v.ID, old, row, p)
	row.inputs = inputs
	if v.Ended == nil {
		row.origin = v.Origin.TriggerID
	}
	if old != nil && old.origin != row.origin && old.origin != "" {
		u.openOrigins = u.openOrigins.set(u.mark, u.originPrefix(old.origin)+row.order, nil)
	}
	if row.origin != "" && (old == nil || old.origin != row.origin) {
		u.openOrigins = u.openOrigins.set(u.mark, u.originPrefix(row.origin)+row.order, &memoryRow{data: []byte(v.ID), order: row.order})
	}
	return nil
}

func (r *memoryView) originPrefix(triggerID string) string {
	if tr := r.triggers.get(triggerID); tr != nil && tr.lookup != "" {
		return "thread" + linkPrefix(tr.lookup)
	}
	return "pending" + linkPrefix(triggerID)
}

func (r *memoryView) OpenOriginWorkID(key string) (string, error) {
	// 倒序取最近插入的一件。在合法的单自有对象上通常只有一项。
	for row := range r.openOrigins.prefixTree("thread" + linkPrefix(key)).reverseRows() {
		return string(row.data), nil
	}
	return "", nil
}

func (u *memoryUnit) PutDecision(v Decision) error {
	previous := u.decisions.get(v.ID)
	old, err := u.GetDecision(v.ID)
	if err != nil {
		return err
	}
	if err = u.put(&u.decisions, v.ID, &v); err != nil {
		return err
	}
	if old != nil {
		u.indexDecision(old, nil)
	}
	row := u.decisions.get(v.ID)
	p := retentionIndex{at: v.At}
	for _, r := range v.Reason {
		p.add("trigger", r.TriggerID, r.Values["trigger"])
		p.add("work", r.Values["work"])
	}
	p.add("thread", v.ThreadKey)
	p.add("trigger", v.TriggerIDs...)
	p.add("work", v.WorkID)
	u.retention("decision", v.ID, previous, row, p)
	u.indexDecision(&v, row)
	return nil
}

func (u *memoryUnit) indexDecision(d *Decision, row *memoryRow) {
	u.decisionsByThread = u.decisionsByThread.set(u.mark, linkPrefix(d.ThreadKey)+d.ID, row)
	if d.Kind == Accept && d.WorkID != "" {
		// 一个批次共享两项定位信息。预检任一事件的开销不依赖历史批次长度。
		if row != nil {
			row = &memoryRow{data: []byte(d.ID), lookup: d.WorkID}
		}
		for _, id := range d.TriggerIDs {
			u.accepted = u.accepted.set(u.mark, linkPrefix(id)+d.ID, row)
		}
	}
}

func (row *memoryRow) decode(dst any) error {
	if err := jsonv2.Unmarshal(row.data, dst); err != nil {
		return fmt.Errorf("解码 %T: %w", dst, err)
	}
	if w, ok := dst.(*Work); ok && row.inputs != nil {
		return row.inputs.load(w)
	}
	return nil
}

func (n *indexNode) readMany(keys []string, dest func(string) any) error {
	for _, key := range keys {
		if row := n.get(key); row != nil {
			if err := row.decode(dest(key)); err != nil {
				return fmt.Errorf("读取键 %q: %w", key, err)
			}
		}
	}
	return nil
}

func (r *memoryView) ActiveThreadsWithPending() ([]Thread, error) {
	return indexedThreads(r.active)
}

func (r *memoryView) EvaluatingThreads() ([]Thread, error) { return indexedThreads(r.byEval) }

func indexedThreads(index *indexNode) ([]Thread, error) {
	var result []Thread
	err := index.walk(func(row *memoryRow) error {
		var th Thread
		if err := row.decode(&th); err != nil {
			return err
		}
		result = append(result, th)
		return nil
	})
	return result, err
}

func (r *memoryView) OpenWorks() ([]Work, error) {
	var result []Work
	err := r.open.walk(func(row *memoryRow) error {
		var w Work
		if err := row.decode(&w); err != nil {
			return err
		}
		result = append(result, w)
		return nil
	})
	slices.Reverse(result)
	return result, err
}

// 长度前缀让分隔符、零字节和互为前缀的对象键均可无歧义地反查。
func linkPrefix(key string) string { return strconv.Itoa(len(key)) + ":" + key }

func (u *memoryUnit) LinkTrigger(triggerID, threadKey string) error {
	key := linkPrefix(threadKey) + triggerID
	if u.triggersByThread.get(key) == nil {
		u.triggersByThread = u.triggersByThread.set(u.mark, key, &memoryRow{data: []byte(triggerID)})
		u.threadsByTrigger = u.threadsByTrigger.set(u.mark, linkPrefix(triggerID)+threadKey, &memoryRow{data: []byte(threadKey)})
	}
	return nil
}

func linkedKeys(index *indexNode, key string) ([]string, error) {
	var keys []string
	err := index.prefixTree(linkPrefix(key)).walk(func(row *memoryRow) error {
		keys = append(keys, string(row.data))
		return nil
	})
	return keys, err
}

func (r *memoryView) TriggerIDs(threadKey string) ([]string, error) {
	return linkedKeys(r.triggersByThread, threadKey)
}

func (r *memoryView) ThreadKeys(triggerID string) ([]string, error) {
	return linkedKeys(r.threadsByTrigger, triggerID)
}

func (r *memoryView) GetTrigger(key string) (*Trigger, error) {
	row := r.triggers.get(key)
	if row == nil {
		return nil, nil
	}
	var v Trigger
	if err := row.decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *memoryView) GetTriggers(keys ...string) (map[string]*Trigger, error) {
	values := make(map[string]*Trigger, len(keys))
	err := r.triggers.readMany(keys, func(key string) any {
		v := new(Trigger)
		values[key] = v
		return v
	})
	return values, err
}

func (r *memoryView) GetDisposition(key string) (*TriggerDisposition, error) {
	row := r.dispositions.get(key)
	if row == nil {
		return nil, nil
	}
	var v TriggerDisposition
	if err := row.decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *memoryView) GetDispositions(keys ...string) (map[string]*TriggerDisposition, error) {
	values := make(map[string]*TriggerDisposition, len(keys))
	err := r.dispositions.readMany(keys, func(key string) any {
		v := new(TriggerDisposition)
		values[key] = v
		return v
	})
	return values, err
}

func (r *memoryView) GetThread(key string) (*Thread, error) {
	row := r.threads.get(key)
	if row == nil {
		return nil, nil
	}
	var v Thread
	if err := row.decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *memoryView) GetThreads(keys ...string) (map[string]*Thread, error) {
	values := make(map[string]*Thread, len(keys))
	err := r.threads.readMany(keys, func(key string) any {
		v := new(Thread)
		values[key] = v
		return v
	})
	return values, err
}

func (r *memoryView) GetWork(key string) (*Work, error) {
	row := r.works.get(key)
	if row == nil {
		return nil, nil
	}
	var v Work
	if err := row.decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *memoryView) GetWorks(keys ...string) (map[string]*Work, error) {
	values := make(map[string]*Work, len(keys))
	err := r.works.readMany(keys, func(key string) any {
		v := new(Work)
		values[key] = v
		return v
	})
	return values, err
}

func (r *memoryView) GetDecision(key string) (*Decision, error) {
	row := r.decisions.get(key)
	if row == nil {
		return nil, nil
	}
	var v Decision
	if err := row.decode(&v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (r *memoryView) GetDecisions(keys ...string) (map[string]*Decision, error) {
	values := make(map[string]*Decision, len(keys))
	err := r.decisions.readMany(keys, func(key string) any {
		v := new(Decision)
		values[key] = v
		return v
	})
	return values, err
}
