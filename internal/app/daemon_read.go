package app

import (
	"context"
	"encoding/json"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

func (h extensionRoutes) get(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Key string `json:"key"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if _, err := h.fromKey(p.Key); err != nil {
		return nil, err
	}
	var result *engine.Thread
	err := h.runtime.repo.Read(ctx, func(r engine.Reader) error {
		var err error
		result, err = r.GetThread(p.Key)
		return err
	})
	return result, err
}

func (h extensionRoutes) subscribed(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Kind string `json:"kind"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	cutoff := h.runtime.clock.Now().Add(-14 * 24 * time.Hour).UTC().Format(utcMillis)
	result := []engine.Thread{}
	err := h.db.View(ctx, func(read store.SQLiteReader) error {
		for _, src := range h.sources {
			prefix := src
			if p.Kind != "" {
				if !h.kinds[[2]string{src, p.Kind}] {
					continue
				}
				prefix += ":" + p.Kind
			}
			// 冒号的下一个字节是分号，半开区间精确覆盖这个来源或类型的全部键。
			threads, err := (engineReader{read}).threads("subscribed", prefix+":", prefix+";", cutoff)
			if err != nil {
				return err
			}
			result = append(result, threads...)
		}
		return nil
	})
	return result, err
}

func (h extensionRoutes) open(ctx context.Context, _ json.RawMessage) (any, error) {
	type summary struct {
		ID    string       `json:"id"`
		Title string       `json:"title"`
		Stage engine.Stage `json:"stage"`
	}
	result := []summary{}
	err := h.runtime.repo.Read(ctx, func(r engine.Reader) error {
		works, err := r.OpenWorks()
		if err != nil {
			return err
		}
		for _, w := range works {
			result = append(result, summary{w.ID, w.Title, engine.WorkStage(w)})
		}
		return nil
	})
	return result, err
}

// 键和值都按字节限制，否则也可以把整份对象编码在键名里。
const (
	maxStateKeys  = 128
	maxStateKey   = 256
	maxStateValue = 4 << 10
)

func (h extensionRoutes) stateSet(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Key) == 0 || len(p.Key) > maxStateKey || len(p.Value) == 0 || len(p.Value) > maxStateValue {
		return nil, invalid("私有状态键或值的大小无效")
	}
	return nil, h.db.Write(ctx, func(tx *store.SQLiteTx) error {
		ok, err := tx.PutState(h.id, p.Key, p.Value, maxStateKeys)
		if err == nil && !ok {
			return invalid("私有状态键数已达上限")
		}
		return err
	})
}

func (h extensionRoutes) stateGet(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Key string `json:"key"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if p.Key == "" || len(p.Key) > maxStateKey {
		return nil, invalid("状态键无效")
	}
	var value json.RawMessage
	err := h.db.View(ctx, func(r store.SQLiteReader) error {
		rows, err := r.Lookup("stateGet", h.id, p.Key)
		if len(rows) > 0 {
			value = rows[0].Value
		}
		return err
	})
	return value, err
}

func (h extensionRoutes) stateKeys(ctx context.Context, _ json.RawMessage) (any, error) {
	var keys []string
	err := h.db.View(ctx, func(r store.SQLiteReader) error {
		var err error
		keys, err = (engineReader{r}).keys("stateKeys", h.id)
		return err
	})
	if keys == nil {
		keys = []string{}
	}
	return keys, err
}
