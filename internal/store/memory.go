package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/chhuax/craft/internal/store/migrate"
)

type memoryStore struct {
	writing   chan struct{}
	mu        sync.RWMutex
	committed memoryRead
	logger    *slog.Logger
}

// NewMemory 创建供一致性用例使用的内存仓储，logger 为 nil 时使用 slog 默认日志。
func NewMemory(logger *slog.Logger) Store {
	if logger == nil {
		logger = slog.Default()
	}
	return &memoryStore{writing: make(chan struct{}, 1), logger: logger}
}

func (s *memoryStore) snapshot() memoryRead {
	// 发布后的快照只读，只需在取得引用时加锁，读回调不会阻塞写回调。
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.committed
}

func (s *memoryStore) Read(ctx context.Context, fn func(ReadTx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("开始内存读事务: %w", err)
	}
	if err := fn(s.snapshot()); err != nil {
		return fmt.Errorf("执行内存读事务: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("结束内存读事务: %w", err)
	}
	return nil
}

func (s *memoryStore) Atomic(ctx context.Context, fn func(Tx) error) error {
	c, err := s.commit(ctx, fn)
	if err != nil {
		return fmt.Errorf("执行内存写事务: %w", err)
	}
	c.run(s.logger)
	return nil
}

func (s *memoryStore) commit(ctx context.Context, fn func(Tx) error) (callbacks, error) {
	// 写者从取得快照到发布始终串行，但提交回调可以重新进入仓储。
	select {
	case s.writing <- struct{}{}:
		defer func() { <-s.writing }()
	case <-ctx.Done():
		return nil, fmt.Errorf("等候内存写事务: %w", ctx.Err())
	}
	root := memoryTx{memoryRead: s.snapshot()}
	if err := root.Atomic(ctx, fn); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.committed = root.memoryRead
	s.mu.Unlock()
	return root.callbacks, nil
}

type memoryRead map[string]map[string]json.RawMessage

type memoryTx struct {
	memoryRead
	callbacks
}

func (r memoryRead) clone() memoryRead {
	copy := make(memoryRead, len(r))
	for name, docs := range r {
		values := make(map[string]json.RawMessage, len(docs))
		for key, value := range docs {
			values[key] = bytes.Clone(value)
		}
		copy[name] = values
	}
	return copy
}

func (tx *memoryTx) Atomic(ctx context.Context, fn func(Tx) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("开始内存快照: %w", err)
	}
	child := memoryTx{memoryRead: tx.memoryRead.clone()}
	if err := fn(&child); err != nil {
		return fmt.Errorf("执行内存快照事务: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("提交内存快照: %w", err)
	}
	tx.memoryRead = child.memoryRead
	tx.callbacks = append(tx.callbacks, child.callbacks...)
	return nil
}

func (r memoryRead) Get(collection, key string) (json.RawMessage, bool, error) {
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return nil, false, err
	}
	value, ok := r[table][key]
	return bytes.Clone(value), ok, nil
}

func (r memoryRead) List(collection, filter string) (map[string]json.RawMessage, error) {
	table, missing, err := migrate.Resolve(collection, filter)
	if err != nil {
		return nil, err
	}
	result := make(map[string]json.RawMessage)
	for key, value := range r[table] {
		if missing != "" && bytes.TrimSpace(value)[0] == '{' {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(value, &fields); err != nil {
				return nil, fmt.Errorf("筛选 %s[%q]: %w", collection, key, err)
			}
			if _, present := fields[missing]; present {
				continue
			}
		}
		result[key] = bytes.Clone(value)
	}
	return result, nil
}

func (tx *memoryTx) Put(collection, key string, value json.RawMessage) error {
	return tx.PutMany(collection, []Doc{{Key: key, Value: value}})
}

func (tx *memoryTx) PutMany(collection string, docs []Doc) error {
	if len(docs) == 0 {
		return nil
	}
	table, err := validateDocs(collection, docs)
	if err != nil {
		return err
	}
	if tx.memoryRead[table] == nil {
		tx.memoryRead[table] = make(map[string]json.RawMessage)
	}
	for _, doc := range docs {
		tx.memoryRead[table][doc.Key] = bytes.Clone(doc.Value)
	}
	return nil
}

func (tx *memoryTx) Delete(collection, key string) error {
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return err
	}
	delete(tx.memoryRead[table], key)
	return nil
}
