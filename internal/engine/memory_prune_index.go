package engine

import (
	"bytes"
	"encoding/json"
)

// 每次替换文档都撤销此前的日期与引用投影；不必在清理时解码整库历史。
// 投递窗口只改回执字段，可以沿用同一组引用。
type retentionIndex struct {
	at   string
	refs []PruneObject
}

func (p *retentionIndex) add(kind string, ids ...string) {
	for _, id := range ids {
		if id != "" {
			p.refs = append(p.refs, PruneObject{kind, id})
		}
	}
}

func workRetention(w *Work) retentionIndex {
	var p retentionIndex
	if w.Ended != nil {
		p.at = w.Ended.At
	}
	p.add("decision", w.DecisionID)
	p.add("trigger", w.Origin.TriggerID)
	p.add("trigger", w.Origin.TriggerIDs...)
	p.add("trigger", w.Answers...)
	if w.Attention != nil {
		p.add("trigger", w.Attention.TriggerID)
	}
	for _, in := range w.Inputs {
		p.add("trigger", in.TriggerID)
		p.add("decision", in.DecisionID)
	}
	for _, f := range w.Follows {
		p.add("thread", f.Thread)
		p.add("trigger", f.Via...)
	}
	return p
}

func threadRetention(th *Thread) retentionIndex {
	var p retentionIndex
	// 有 pending 或 evaluating 的对象自身也是保护根，包括已关闭的对象。
	if th.Closed != nil && len(th.Pending) == 0 && th.Evaluating == nil {
		p.at = th.Closed.At
	}
	p.add("thread", th.Correlated...)
	p.add("trigger", th.Pending...)
	if ev := th.Evaluating; ev != nil {
		p.add("thread", ev.ThreadKey)
		p.add("trigger", ev.Primary)
		p.add("trigger", ev.TriggerIDs...)
	}
	return p
}

func dispositionRetention(d *TriggerDisposition) retentionIndex {
	var p retentionIndex
	if d.Filtered != nil {
		p.add("trigger", d.Filtered.TriggerID)
	}
	p.add("decision", d.DecisionID)
	p.add("work", d.Fanout...)
	p.add("work", d.Context...)
	for _, h := range d.Handbacks {
		p.add("work", h.WorkID)
	}
	return p
}

func effectRetention(kind, key string, raw json.RawMessage) (retentionIndex, error) {
	var p retentionIndex
	if kind == "delivery" {
		p.add("work", key)
	}
	// reply 的消息 ID 可由事件 ID 派生，但执行只读载荷里的渠道与正文。
	// 只解释载荷顶层的 Watch 引用，渠道、会话及扩展字段各有自己的命名空间。
	if len(raw) == 0 || raw[0] != '{' {
		return p, nil
	}
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return p, err
	}
	for field, kind := range map[string]string{"workId": "work", "decisionId": "decision", "threadKey": "thread", "triggerId": "trigger"} {
		if id, ok := fields[field].(string); ok {
			p.add(kind, id)
		}
	}
	if ids, ok := fields["triggerIds"].([]any); ok {
		for _, value := range ids {
			if id, ok := value.(string); ok {
				p.add("trigger", id)
			}
		}
	}
	return p, nil
}

func (u *memoryUnit) retention(kind, id string, old, row *memoryRow, p retentionIndex) {
	src := PruneObject{kind, id}
	if kind == "disposition" {
		src.Kind = "trigger"
	}
	change := func(index *retentionIndex, entry *memoryRow) {
		if index == nil {
			return
		}
		if index.at != "" {
			u.pruneDates = u.pruneDates.set(u.mark, kind+"\x00"+index.at+"\x00"+id, entry)
		}
		for _, target := range index.refs {
			key := target.Kind + "\x00" + linkPrefix(target.ID) + kind + "\x00" + id
			u.pruneRefs = u.pruneRefs.set(u.mark, key, entry)
		}
	}
	if old != nil {
		change(old.retention, nil)
	}
	if row != nil && (p.at != "" || len(p.refs) > 0) {
		row.retention = &p
		change(&p, &memoryRow{lookup: src.Kind, data: []byte(src.ID)})
	}
}
