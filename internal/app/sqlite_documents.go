package app

import (
	"encoding/json/v2"
	"fmt"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

func (u *engineUnit) put(collection, key string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("编码 %s[%q]: %w", collection, key, err)
	}
	return u.tx.Put(collection, key, raw)
}

func (u *engineUnit) PutTrigger(v engine.Trigger) error {
	// 重放保留第一次的事实，即使新负载已经无法编码。
	if _, ok, err := u.read.Get("watch_trigger", v.ID); err != nil || ok {
		return err
	}
	return u.put("watch_trigger", v.ID, &v)
}

func (u *engineUnit) PutDisposition(id string, v engine.TriggerDisposition) error {
	return u.put("watch_disposition", id, &v)
}

func (u *engineUnit) PutThread(v engine.Thread) error     { return u.put("watch_thread", v.Key, &v) }
func (u *engineUnit) PutWork(v engine.Work) error         { return u.put("watch_work", v.ID, &v) }
func (u *engineUnit) PutDecision(v engine.Decision) error { return u.put("watch_decision", v.ID, &v) }

func (u *engineUnit) LinkTrigger(id, key string) error {
	// JSON 数组同时充当复合键与文档，不会因分量含冒号或零字节而串联。
	raw, err := json.Marshal([2]string{id, key})
	if err != nil {
		return fmt.Errorf("编码事件关联 %q / %q: %w", id, key, err)
	}
	return u.tx.Put("watch_trigger_thread", string(raw), raw)
}

func (r engineReader) get(collection, key string, dst any) error {
	raw, ok, err := r.read.Get(collection, key)
	if err != nil || !ok {
		return err
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("解码 %s[%q]: %w", collection, key, err)
	}
	return nil
}

func (r engineReader) many(collection string, keys []string, dest func(string) any) error {
	rows, err := r.read.GetMany(collection, keys)
	if err != nil {
		return err
	}
	return decodeDocs(rows, func(i int) any { return dest(rows[i].Key) })
}

func decodeDocs(rows []store.Doc, dest func(int) any) error {
	for i, row := range rows {
		if err := json.Unmarshal(row.Value, dest(i)); err != nil {
			return fmt.Errorf("解码文档 %q: %w", row.Key, err)
		}
	}
	return nil
}

func (r engineReader) GetTrigger(id string) (*engine.Trigger, error) {
	var v *engine.Trigger
	err := r.get("watch_trigger", id, &v)
	return v, err
}

func (r engineReader) GetTriggers(ids ...string) (map[string]*engine.Trigger, error) {
	result := make(map[string]*engine.Trigger, len(ids))
	err := r.many("watch_trigger", ids, func(key string) any {
		v := new(engine.Trigger)
		result[key] = v
		return v
	})
	return result, err
}

func (r engineReader) GetDisposition(id string) (*engine.TriggerDisposition, error) {
	var v *engine.TriggerDisposition
	err := r.get("watch_disposition", id, &v)
	return v, err
}

func (r engineReader) GetDispositions(ids ...string) (map[string]*engine.TriggerDisposition, error) {
	result := make(map[string]*engine.TriggerDisposition, len(ids))
	err := r.many("watch_disposition", ids, func(key string) any {
		v := new(engine.TriggerDisposition)
		result[key] = v
		return v
	})
	return result, err
}

func (r engineReader) GetThread(id string) (*engine.Thread, error) {
	var v *engine.Thread
	err := r.get("watch_thread", id, &v)
	return v, err
}

func (r engineReader) GetThreads(ids ...string) (map[string]*engine.Thread, error) {
	result := make(map[string]*engine.Thread, len(ids))
	err := r.many("watch_thread", ids, func(key string) any {
		v := new(engine.Thread)
		result[key] = v
		return v
	})
	return result, err
}

func (r engineReader) GetWork(id string) (*engine.Work, error) {
	var v *engine.Work
	err := r.get("watch_work", id, &v)
	return v, err
}

func (r engineReader) GetWorks(ids ...string) (map[string]*engine.Work, error) {
	result := make(map[string]*engine.Work, len(ids))
	err := r.many("watch_work", ids, func(key string) any {
		v := new(engine.Work)
		result[key] = v
		return v
	})
	return result, err
}

func (r engineReader) GetDecision(id string) (*engine.Decision, error) {
	var v *engine.Decision
	err := r.get("watch_decision", id, &v)
	return v, err
}

func (r engineReader) GetDecisions(ids ...string) (map[string]*engine.Decision, error) {
	result := make(map[string]*engine.Decision, len(ids))
	err := r.many("watch_decision", ids, func(key string) any {
		v := new(engine.Decision)
		result[key] = v
		return v
	})
	return result, err
}
