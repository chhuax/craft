package app

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
)

type sqliteOutbox struct {
	db     *store.SQLite
	repo   *engineStore
	health *daemonHealth
}

// NewSQLiteOutbox 将已有文件库适配为外发仓储；其连接仍由调用方关闭。
func NewSQLiteOutbox(db *store.SQLite) outbound.Outbox { return sqliteOutbox{db: db} }

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
		var err error
		result, err = outboxRecords(tx, state)
		return err
	})
	return result, err
}

func outboxRecords(tx store.SQLiteReader, state outbound.State) ([]outbound.Result, error) {
	name := "outboxList"
	var args []any
	if state != "" {
		name, args = "outboxState", []any{string(state)}
	}
	docs, err := tx.Lookup(name, args...)
	if err != nil {
		return nil, err
	}
	result := make([]outbound.Result, len(docs))
	err = decodeDocs(docs, func(i int) any { return &result[i] })
	return result, err
}

func (s sqliteOutbox) Save(ctx context.Context, r *outbound.Result, rate outbound.RateLimit, clock func() time.Time) error {
	var from outbound.State
	changed := false
	return s.db.WriteAndPublish(ctx, func(tx *store.SQLiteTx) error {
		current, err := outboxRow(tx, r.ID)
		if err != nil {
			return err
		}
		from = current.State
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
		if r.State == outbound.Done {
			// 在完成事务分配序号；首次建单顺序和墙钟都不能用作回执游标。
			r.SentSeq, err = tx.NextCounter("outbox.sentSeq")
			if err != nil {
				return err
			}
		}
		if s.repo != nil && (from == outbound.Pending || r.State == outbound.Pending) {
			if err := s.repo.in(ctx, tx).Emit(engine.Event{Type: "outbox.changed", At: r.At.UTC().Format(utcMillis), Ref: map[string]string{"outbox": r.ID}}); err != nil {
				return err
			}
		}
		if s.health != nil {
			tx.AfterCommit(func() { s.health.notify(changed) })
		}
		raw, err := json.Marshal(r)
		if err != nil {
			return fmt.Errorf("编码外发记录 %q: %w", r.ID, err)
		}
		return tx.Put("outbox", r.ID, raw)
	}, func() {
		// 同一写连接按提交顺序发布计数，避免并发恢复或确认产生过期快照。
		if s.health != nil {
			changed = s.health.outboxChange(from, r.State, 1)
		}
	})
}
