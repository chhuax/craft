package engine

import (
	"encoding/json"
	"encoding/json/jsontext"
	"slices"
)

// 保留首次命中的分发顺序。同一工作可以经多个对象到达，只要有一条正常订阅便能认领。
func subscribers(keys []string, threads map[string]*Thread) ([]string, map[string]bool) {
	var ids []string
	contextOnly := make(map[string]bool)
	for _, key := range keys {
		th := threads[key]
		context := make(map[string]bool, len(th.Context))
		for _, id := range th.Context {
			context[id] = true
		}
		for _, id := range th.Subscriptions {
			previous, seen := contextOnly[id]
			if !seen {
				ids = append(ids, id)
			}
			contextOnly[id] = (!seen || previous) && context[id]
		}
	}
	return ids, contextOnly
}

func fanoutTrigger(u Unit, tr Trigger, keys []string, threads map[string]*Thread, p Policy, d *TriggerDisposition) (map[string]*Work, string, error) {
	ids, contextOnly := subscribers(keys, threads)
	if len(ids) == 0 {
		return nil, "", nil
	}
	works, err := u.GetWorks(ids...)
	if err != nil {
		return nil, "", err
	}
	var skipped string
	mode := p.ModeOf(tr.Source, tr.Event)
	for _, id := range ids {
		w := works[id]
		if w == nil {
			continue
		}
		if w.Ended != nil {
			if !tr.Human || w.Ended.By == "human" || contextOnly[id] {
				continue
			}
			if !p.Revalidate(w.Mandate) {
				skipped = CodeMandateRevoked
				continue
			}
			if mode == ModeOff {
				continue
			}
		}
		if err := deliverInput(u, w, tr, contextOnly[id], threads, d.At); err != nil {
			return nil, "", err
		}
		d.Fanout = append(d.Fanout, id)
		if contextOnly[id] {
			d.Context = append(d.Context, id)
		}
	}
	return works, skipped, nil
}

func deliverInput(u Unit, w *Work, tr Trigger, contextOnly bool, threads map[string]*Thread, now string) error {
	id := Key("input", w.ID, "trigger", tr.ID)
	w.Inputs = append(w.Inputs, WorkInput{Kind: "trigger", ID: id, TriggerID: tr.ID, At: now, Human: tr.Human, Context: contextOnly})
	w.Undelivered = append(w.Undelivered, id)
	w.UpdatedAt = now
	resuming := w.Ended != nil
	if resuming {
		w.Ended = nil
		// 结束期间对象可以重开。按本次已读的对象事实修正跟随状态。
		for i := range w.Follows {
			f := &w.Follows[i]
			if th := threads[f.Thread]; th != nil {
				f.Closed = th.Closed
			}
		}
		resumed := WorkInput{ID: Key("input", w.ID, "resumed", tr.ID), Kind: "resumed", TriggerID: tr.ID, At: now}
		w.Inputs = append(w.Inputs, resumed)
		// 历史按产生顺序追加，投递时先说明恢复缘由，再送待处理输入。
		w.Undelivered = slices.Insert(w.Undelivered, 0, resumed.ID)
	}
	if err := u.PutWork(*w); err != nil {
		return err
	}
	if resuming {
		return emitResumed(u, w.ID, tr.ID, now)
	}
	return nil
}

func emitResumed(u Unit, work, trigger, now string) error {
	workID, err := jsontext.AppendQuote(nil, work)
	if err != nil {
		return err
	}
	triggerID, err := jsontext.AppendQuote(nil, trigger)
	if err != nil {
		return err
	}
	return u.Emit(Event{Type: "work.resumed", At: now, Ref: map[string]string{"work": work, "trigger": trigger},
		Data: map[string]json.RawMessage{"workId": workID, "triggerId": triggerID}})
}

func emitReceived(u Unit, trigger string, keys []string, now string) error {
	id, err := jsontext.AppendQuote(nil, trigger)
	if err != nil {
		return err
	}
	raw := []byte{'['}
	for i, key := range keys {
		if i != 0 {
			raw = append(raw, ',')
		}
		raw, err = jsontext.AppendQuote(raw, key)
		if err != nil {
			return err
		}
	}
	return u.Emit(Event{Type: "trigger.received", At: now, Ref: map[string]string{"trigger": trigger, "thread": keys[0]},
		Data: map[string]json.RawMessage{"triggerId": id, "threads": append(raw, ']')}})
}
