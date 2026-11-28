package app

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"strconv"
	"sync"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

type engineStore struct {
	onEvent func()
	db      *store.SQLite
	mu      sync.Mutex
	subs    []func(engine.Event)
}

// NewSQLiteStore 将已打开的文件库用作引擎仓储，连接的生命周期仍由调用方管理。
func NewSQLiteStore(db *store.SQLite) engine.Store { return &engineStore{db: db} }

func (s *engineStore) Read(ctx context.Context, fn func(engine.Reader) error) error {
	return s.db.View(ctx, func(r store.SQLiteReader) error { return fn(engineReader{r}) })
}

type engineReader struct{ read store.SQLiteReader }
type engineUnit struct {
	engineReader
	tx    *store.SQLiteTx
	store *engineStore
	ctx   context.Context
}

func (s *engineStore) unit(ctx context.Context, fn func(engine.Unit) error) func(*store.SQLiteTx) error {
	return func(tx *store.SQLiteTx) error { return fn(s.in(ctx, tx)) }
}

func (s *engineStore) in(ctx context.Context, tx *store.SQLiteTx) *engineUnit {
	return &engineUnit{engineReader{tx.SQLiteReader}, tx, s, ctx}
}

func (s *engineStore) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return s.db.Write(ctx, s.unit(ctx, fn))
}

func (u *engineUnit) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return u.tx.Savepoint(ctx, u.store.unit(ctx, fn))
}

func (u *engineUnit) AfterCommit(fn func())   { u.tx.AfterCommit(fn) }
func (u *engineUnit) Effects() engine.Effects { return u.tx }

func (s *engineStore) Subscribe(fn func(engine.Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

func (u *engineUnit) Emit(e engine.Event) error {
	// 即使调用方处理编码错误后继续提交，失败的追加也不能推进事件游标。
	return u.tx.Savepoint(u.ctx, func(t *store.SQLiteTx) error {
		rev, err := t.NextCounter("event.rev")
		if err != nil {
			return err
		}
		e.Rev = rev
		raw, err := json.Marshal(&e)
		if err != nil {
			return fmt.Errorf("编码事件 %q: %w", e.Type, err)
		}
		if err := t.Put("watch_event", strconv.FormatInt(rev, 10), raw); err != nil {
			return err
		}
		s := u.store
		t.AfterCommit(func() { s.broadcast(raw) })
		return nil
	})
}

func (s *engineStore) broadcast(raw []byte) {
	if s.onEvent != nil {
		s.onEvent()
	}
	s.mu.Lock()
	subs := s.subs
	s.mu.Unlock()
	// 订阅表只追加。回调可重入仓储，每个订阅者获得独占的嵌套字段。
	for _, fn := range subs {
		deliverEvent(fn, raw)
	}
}

func deliverEvent(fn func(engine.Event), raw []byte) {
	defer func() {
		if failure := recover(); failure != nil {
			slog.Error("引擎事件订阅者失败", "panic", failure)
		}
	}()
	var e engine.Event
	if err := json.Unmarshal(raw, &e); err != nil {
		slog.Error("解码已提交的事件失败", "error", err)
		return
	}
	fn(e)
}

// Prune 复用命令已经持有的写事务，使引用保护与级联同时生效。
func (u *engineUnit) Prune(triggerBefore, workBefore, eventBefore string) (engine.PruneResult, error) {
	return u.tx.Prune(triggerBefore, workBefore, eventBefore)
}
