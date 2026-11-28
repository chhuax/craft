package engine

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
)

type memoryStore struct {
	writing     chan struct{}
	committed   atomic.Pointer[memoryView]
	mu          sync.Mutex
	subscribers []func(Event)
}

// NewMemory 创建独立的内存仓储。快照随最后一个读回调释放，无需关闭资源。
func NewMemory() Store {
	s := &memoryStore{writing: make(chan struct{}, 1)}
	s.committed.Store(&memoryView{})
	return s
}

type memoryView struct {
	triggers, dispositions, threads, works, decisions *indexNode
	active, open, triggersByThread, threadsByTrigger  *indexNode
	byEval, recentOpen, accepted, decisionsByThread   *indexNode
	subscribedThreads, openOrigins                    *indexNode
	insertions                                        uint64
	rev                                               int64
	events, pruneDates, pruneRefs                     *indexNode
	effects                                           []Effect
	effectKeys, replyStates                           *indexNode
}

type memoryUnit struct {
	memoryView
	store *memoryStore
	mark  *writeMark
	after []func()
}

func (s *memoryStore) Read(ctx context.Context, fn func(Reader) error) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("开始内存读事务: %w", err)
	}
	if err := fn(s.committed.Load()); err != nil {
		return fmt.Errorf("执行内存读事务: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("结束内存读事务: %w", err)
	}
	return nil
}

func (s *memoryStore) Atomic(ctx context.Context, fn func(Unit) error) error {
	after, err := s.commit(ctx, fn)
	if err != nil {
		return fmt.Errorf("内存写事务: %w", err)
	}
	for _, fn := range after {
		afterCommit(fn)
	}
	return nil
}

func (s *memoryStore) commit(ctx context.Context, fn func(Unit) error) ([]func(), error) {
	select {
	case s.writing <- struct{}{}:
		defer func() { <-s.writing }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	u := memoryUnit{memoryView: *s.committed.Load(), store: s, mark: new(writeMark)}
	if err := u.run(ctx, fn); err != nil {
		return nil, err
	}
	// 发布的数据与事务分开持有，避免快照延长已执行回调及其闭包的寿命。
	view := u.memoryView
	s.committed.Store(&view)
	return u.after, nil
}

func (u *memoryUnit) run(ctx context.Context, fn func(Unit) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(u); err != nil {
		return err
	}
	return ctx.Err()
}

func (u *memoryUnit) Atomic(ctx context.Context, fn func(Unit) error) error {
	child := memoryUnit{memoryView: u.memoryView, store: u.store, mark: new(writeMark)}
	if err := child.run(ctx, fn); err != nil {
		return fmt.Errorf("内存保存点: %w", err)
	}
	u.memoryView = child.memoryView
	u.after = append(u.after, child.after...)
	return nil
}

func (u *memoryUnit) AfterCommit(fn func()) { u.after = append(u.after, fn) }

func afterCommit(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			slog.Error("引擎提交回调失败", "panic", v)
		}
	}()
	fn()
}

func (s *memoryStore) Subscribe(fn func(Event)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subscribers = append(s.subscribers, fn)
}
