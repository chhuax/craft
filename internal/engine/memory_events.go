package engine

import (
	"bytes"
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
	u.events = append(u.events, data)
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
	start := max(rev, 0)
	if limit <= 0 || start >= int64(len(r.events)) {
		return nil, nil
	}
	count := min(limit, len(r.events)-int(start))
	out := make([]Event, count)
	for i, raw := range r.events[int(start) : int(start)+count] {
		if err := jsonv2.Unmarshal(raw, &out[i]); err != nil {
			return nil, fmt.Errorf("解码事件 %d: %w", start+int64(i)+1, err)
		}
	}
	return out, nil
}
