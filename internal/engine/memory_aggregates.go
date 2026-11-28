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

func (u *memoryUnit) putIndexed(tree, index **indexNode, key string, v any, active bool) error {
	raw, err := jsonv2.Marshal(v)
	if err != nil {
		return fmt.Errorf("编码 %T[%q]: %w", v, key, err)
	}
	row := &memoryRow{data: raw}
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
	return u.put(&u.triggers, v.ID, &v)
}

func (u *memoryUnit) PutDisposition(id string, v TriggerDisposition) error {
	return u.put(&u.dispositions, id, &v)
}

func (u *memoryUnit) PutThread(v Thread) error {
	return u.putIndexed(&u.threads, &u.active, v.Key, &v, v.Closed == nil && v.Evaluating == nil && len(v.Pending) > 0)
}

func (u *memoryUnit) PutWork(v Work) error {
	return u.putIndexed(&u.works, &u.open, v.ID, &v, v.Ended == nil)
}

func (u *memoryUnit) PutDecision(v Decision) error { return u.put(&u.decisions, v.ID, &v) }

func (row *memoryRow) decode(dst any) error {
	if err := jsonv2.Unmarshal(row.data, dst); err != nil {
		return fmt.Errorf("解码 %T: %w", dst, err)
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
	var result []Thread
	err := r.active.walk(func(row *memoryRow) error {
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
