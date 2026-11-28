package app

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
)

type sqliteOutbox struct{ db *store.SQLite }

// NewSQLiteOutbox 将已有文件库适配为外发仓储；其连接仍由调用方关闭。
func NewSQLiteOutbox(db *store.SQLite) outbound.Outbox { return sqliteOutbox{db} }

func outboxRow(tx store.ReadTx, id string) (outbound.Result, error) {
	raw, ok, err := tx.Get("outbox", id)
	if err != nil || !ok {
		return outbound.Result{}, err
	}
	var r outbound.Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("解码外发记录 %q: %w", id, err)
	}
	return r, nil
}

func (s sqliteOutbox) Get(ctx context.Context, id string) (r outbound.Result, err error) {
	err = s.db.Read(ctx, func(tx store.ReadTx) error {
		var readErr error
		r, readErr = outboxRow(tx, id)
		return readErr
	})
	return r, err
}

func (s sqliteOutbox) Records(ctx context.Context, state outbound.State) ([]outbound.Result, error) {
	var result []outbound.Result
	err := s.db.View(ctx, func(tx store.SQLiteReader) error {
		var docs []store.Doc
		if state != "" {
			rows, err := tx.Lookup("outboxState", string(state))
			if err != nil {
				return err
			}
			docs = rows
		} else {
			all, err := tx.List("outbox", store.All)
			if err != nil {
				return err
			}
			for key, raw := range all {
				docs = append(docs, store.Doc{Key: key, Value: raw})
			}
		}
		for _, doc := range docs {
			var r outbound.Result
			if err := json.Unmarshal(doc.Value, &r); err != nil {
				return fmt.Errorf("解码外发记录 %q: %w", doc.Key, err)
			}
			result = append(result, r)
		}
		return nil
	})
	return result, err
}

func (s sqliteOutbox) Save(ctx context.Context, r *outbound.Result, rate outbound.RateLimit, clock func() time.Time) error {
	return s.db.Write(ctx, func(tx *store.SQLiteTx) error {
		current, err := outboxRow(tx, r.ID)
		if err != nil {
			return err
		}
		if err := current.PrepareTransition(r, rate, clock()); err != nil {
			return err
		}
		// 名额与执行中标记同事务落盘，另一个进程不必等回执就能看见占用。
		if r.State == outbound.Executing && r.AutoAt != nil {
			rows, err := tx.Lookup("autoWindow", *r.AutoAt-rate.Window.Milliseconds(), *r.AutoAt, rate.Max-1)
			if err != nil {
				return err
			}
			if len(rows) > 0 {
				return outbound.ErrRateLimited
			}
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("编码外发记录 %q: %w", r.ID, err)
		}
		return tx.Put("outbox", r.ID, raw)
	})
}
