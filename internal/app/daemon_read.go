package app

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
)

func (h extensionRoutes) get(ctx context.Context, key string) (any, error) {
	var result *engine.Thread
	err := h.runtime.repo.Read(ctx, func(r engine.Reader) error {
		var err error
		result, err = r.GetThread(key)
		return err
	})
	return result, err
}

func (h extensionRoutes) subscribed(ctx context.Context, kind string) (any, error) {
	cutoff := h.runtime.clock.Now().Add(-14 * 24 * time.Hour).UTC().Format(utcMillis)
	result := []engine.Thread{}
	err := h.db.View(ctx, func(read store.SQLiteReader) error {
		for _, src := range h.sources {
			prefix := src
			if kind != "" {
				if _, ok := h.scope.Kind(src, kind); !ok {
					continue
				}
				prefix += ":" + kind
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

func (h extensionRoutes) open(ctx context.Context) (any, error) {
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

func (h extensionRoutes) stateSet(ctx context.Context, p *exthost.SetState) (any, error) {
	return nil, h.db.Write(ctx, func(tx *store.SQLiteTx) error {
		ok, err := tx.PutState(h.id, p.Key, p.Value, exthost.MaxStateKeys)
		if err == nil && !ok {
			return fmt.Errorf("%w: 私有状态键数已达上限", exthost.ErrInvalidParams)
		}
		return err
	})
}

func (h extensionRoutes) stateGet(ctx context.Context, key string) (any, error) {
	var value json.RawMessage
	err := h.db.View(ctx, func(r store.SQLiteReader) error {
		rows, err := r.Lookup("stateGet", h.id, key)
		if len(rows) > 0 {
			value = rows[0].Value
		}
		return err
	})
	return value, err
}

func (h extensionRoutes) stateKeys(ctx context.Context) (any, error) {
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
