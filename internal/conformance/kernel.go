package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

type kernel struct {
	scope   exthost.SourceScope
	now     string
	mu      sync.Mutex
	state   map[string]json.RawMessage
	inbox   map[string]bool
	threads map[string]*thread
}

type thread struct {
	Key        string             `json:"key"`
	Ref        protocol.ThreadRef `json:"ref"`
	CreatedAt  string             `json:"createdAt"`
	Meta       map[string]any     `json:"meta"`
	Closed     *exthost.Closed    `json:"-"`
	ReopenedAt string             `json:"reopenedAt,omitempty"`
}

func newKernel(scope exthost.SourceScope, now time.Time) *kernel {
	return &kernel{scope: scope, now: now.UTC().Format("2006-01-02T15:04:05.000Z"), state: map[string]json.RawMessage{}, inbox: map[string]bool{}, threads: map[string]*thread{}}
}

func key(ref protocol.ThreadRef) string { return ref.Source + ":" + ref.Kind + ":" + ref.ID }

func (k *kernel) getOrCreate(ref protocol.ThreadRef) *thread {
	id := key(ref)
	if t := k.threads[id]; t != nil {
		return t
	}
	t := &thread{Key: id, Ref: ref, CreatedAt: k.now, Meta: map[string]any{"facts": map[string]*exthost.Fact{}}}
	k.threads[id] = t
	return t
}

func (k *kernel) handle(_ context.Context, method string, raw json.RawMessage) (any, error) {
	arg, err := k.scope.Decode(method, raw)
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	switch p := arg.(type) {
	case *protocol.Trigger:
		if k.inbox[p.ID] {
			return "duplicate", nil
		}
		k.inbox[p.ID] = true
		return "queued", nil
	case *exthost.SetState:
		if _, ok := k.state[p.Key]; !ok && len(k.state) >= exthost.MaxStateKeys {
			return nil, fmt.Errorf("%w: 私有状态键数已达上限", exthost.ErrInvalidParams)
		}
		k.state[p.Key] = p.Value
	case *exthost.GetState:
		return k.state[p.Key], nil
	case *exthost.Describe:
		t := k.getOrCreate(*p.Ref)
		if p.Label != nil {
			t.Meta["label"] = *p.Label
		}
		if p.URL != nil {
			t.Meta["url"] = *p.URL
		}
		facts := t.Meta["facts"].(map[string]*exthost.Fact)
		for id, v := range p.Facts {
			if v == nil {
				delete(facts, id)
			} else {
				facts[id] = v
			}
		}
	case *exthost.Patch:
		if t := k.threads[p.Key]; t != nil {
			for _, field := range []string{"url", "label"} {
				if v, ok := p.Meta[field]; ok {
					if string(v) == "null" {
						delete(t.Meta, field)
					} else {
						t.Meta[field] = v
					}
				}
			}
		}
	case *exthost.Closed:
		k.updateStatus(p.Reopened, p)
	case *exthost.Reopened:
		k.updateStatus(*p, nil)
	case *exthost.GetThread:
		t := k.threads[p.Key]
		if t == nil {
			return nil, nil
		}
		// 返回独立快照，连接在锁外编码时其他请求仍可更新元信息。
		v := map[string]any{"key": t.Key, "ref": t.Ref, "createdAt": t.CreatedAt, "meta": t.Meta, "subscriptions": nil, "context": nil, "correlated": nil, "pending": nil}
		if t.Closed != nil {
			v["closed"] = map[string]string{"outcome": t.Closed.Outcome, "at": t.Closed.At, "detail": t.Closed.Detail}
		}
		if t.ReopenedAt != "" {
			v["reopenedAt"] = t.ReopenedAt
		}
		data, err := json.Marshal(v)
		return json.RawMessage(data), err
	case *exthost.Bind:
		// fixture 没有注入 Work；与真内核空库一致，任意外部 workId 都是未知工作。
		return nil, errors.New("unknownWork")
	case *exthost.RecentReceipts:
		return map[string]any{"items": []any{}}, nil
	case *exthost.Subscribed:
		return []any{}, nil
	case *exthost.HealthReport:
		return nil, nil
	default:
		if method == "state/keys" {
			keys := slices.AppendSeq(make([]string, 0, len(k.state)), maps.Keys(k.state))
			slices.Sort(keys)
			return keys, nil
		}
		if method == "work/open" {
			return []any{}, nil
		}
		return nil, exthost.ErrMethodNotFound
	}
	return nil, nil
}

func (k *kernel) updateStatus(p exthost.Reopened, closed *exthost.Closed) {
	t := k.getOrCreate(p.Thread)
	// Decode 已规范化到相同宽度的 UTC 毫秒，同毫秒的关闭优先于重开。
	when := p.At
	last := t.ReopenedAt
	if t.Closed != nil {
		last = max(last, t.Closed.At)
	}
	if when < last || when == last && closed == nil {
		return
	}
	if closed != nil {
		closed.At = when
	} else {
		t.ReopenedAt = when
	}
	t.Closed = closed
}
