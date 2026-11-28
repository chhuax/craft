package engine

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"log/slog"
)

func (u *memoryUnit) Effects() Effects { return u }

func (u *memoryUnit) Enqueue(kind, key string, value json.RawMessage) error {
	if !jsontext.Value(value).IsValid() {
		return fmt.Errorf("effect %s[%q] 的值不是有效 JSON", kind, key)
	}
	p, err := effectRetention(kind, key, bytes.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("读取 %s 请求的引用: %w", kind, err)
	}
	id := linkPrefix(kind) + key
	row := new(memoryRow)
	u.retention("effect", id, u.effectKeys.get(id), row, p)
	u.effectKeys = u.effectKeys.set(u.mark, id, row)
	u.effects = append(u.effects, Effect{Kind: kind, Key: key, Value: bytes.Clone(value)})
	return nil
}

func (u *memoryUnit) Emit(e Event) error {
	e.Rev = u.rev + 1
	data, err := jsonv2.Marshal(&e)
	if err != nil {
		return fmt.Errorf("编码事件 %q: %w", e.Type, err)
	}
	// 计数器与行同属这个写快照。即使调用方捕获编码错误并继续事务，也不会跳号。
	u.rev = e.Rev
	key := revisionKey(e.Rev)
	row := &memoryRow{data: data}
	u.events = u.events.set(u.mark, key, row)
	u.retention("event", key, nil, row, retentionIndex{at: e.At})
	s := u.store
	u.AfterCommit(func() { s.broadcast(data) })
	return nil
}

func (s *memoryStore) broadcast(data []byte) {
	s.mu.Lock()
	subs := s.subscribers
	s.mu.Unlock()
	// 订阅表只追加，已取出的前缀不变。每位订阅者独占一次解码结果，也可重入仓储。
	for _, sub := range subs {
		afterCommit(func() {
			var e Event
			if err := jsonv2.Unmarshal(data, &e); err != nil {
				slog.Error("解码已提交的事件失败", "error", err)
				return
			}
			sub(e)
		})
	}
}

func (r *memoryView) EventsSince(rev int64, limit int) ([]Event, error) {
	if limit <= 0 || rev >= r.rev {
		return nil, nil
	}
	var out []Event
	for _, row := range r.events.between(revisionKey(max(rev, 0)+1), "") {
		var e Event
		if err := jsonv2.Unmarshal(row.data, &e); err != nil {
			return nil, fmt.Errorf("读取修订 %d 之后的事件: %w", rev, err)
		}
		out = append(out, e)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func revisionKey(rev int64) string { return string(binary.BigEndian.AppendUint64(nil, uint64(rev))) }

func (r *memoryView) HasEffect(kind, key string) (bool, error) {
	return r.effectKeys.get(linkPrefix(kind)+key) != nil, nil
}

func (r *memoryView) GetReplyStatus(id string) (ReplyStatus, error) {
	if row := r.replyStates.get(id); row != nil {
		return ReplyStatus(row.lookup), nil
	}
	return "", nil
}
func (u *memoryUnit) PutReplyStatus(id string, status ReplyStatus) error {
	switch status {
	case ReplyPending, ReplySent, ReplyDropped, ReplyConfirm, ReplyFailed, ReplyUnknown:
		u.replyStates = u.replyStates.set(u.mark, id, &memoryRow{lookup: string(status)})
		return nil
	default:
		return fmt.Errorf("回复 %q 的状态无效 %q", id, status)
	}
}
