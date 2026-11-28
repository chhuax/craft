package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/store"
)

// EffectLayer 在 Pass 启动前启动，在 Pass 退出后停止。只有 daemon 实例消费队列。
type EffectLayer interface {
	Start(context.Context) error
	Kick()
	Stop(context.Context) error
}

// EffectHandler 的 Kind 固定，Limit 在每轮调度时重算。Run 可并发执行，
// 须支持取消及幂等重放，并通过引擎命令回报领域结果。
type EffectHandler interface {
	Kind() string
	Limit() int
	Ordered() bool
	Quiet() bool
	Run(context.Context, store.Effect) EffectResult
}

// EffectResult 的零值表示 OK，Later 只推迟到期时间；Err 非空时按失败退避。
type EffectResult struct {
	Later bool
	Err   error
}

// HealthState 是一个 kind 的队列快照，Status 取 ok、paused 或 error。
type HealthState struct {
	store.EffectStats
	Status string
}

type effectScheduler struct {
	db                  *store.SQLite
	clock               alarm.Clock
	retryBase, retryMax time.Duration
	onHealth            func(string, HealthState)
	handlers            []EffectHandler
	wake                chan struct{}

	// 只保护启停与提交执行名额。库操作、处理器及健康回调都在锁外。
	mu     sync.Mutex
	active *effectRun
}

// NewEffectScheduler 使用与 store.WithClock 相同的时钟，重试间隔以库的毫秒精度为准。
// onHealth 可为空；回调由调度循环串行通知状态变化，应及时返回，可调用 Kick。
func NewEffectScheduler(db *store.SQLite, clock alarm.Clock, retryBase, retryMax time.Duration,
	onHealth func(string, HealthState), handlers ...EffectHandler) (EffectLayer, error) {
	if db == nil || clock == nil {
		return nil, errors.New("effect 调度需要数据库和时钟")
	}
	if retryBase < time.Millisecond || retryMax < retryBase {
		return nil, errors.New("effect 重试间隔无效")
	}
	seen := make(map[string]bool, len(handlers))
	for _, h := range handlers {
		if h == nil || h.Kind() == "" {
			return nil, errors.New("effect 处理器缺少 kind")
		}
		if seen[h.Kind()] {
			return nil, fmt.Errorf("effect 处理器 %q 重复", h.Kind())
		}
		seen[h.Kind()] = true
	}
	return &effectScheduler{db: db, clock: clock, retryBase: retryBase, retryMax: retryMax, onHealth: onHealth,
		handlers: slices.Clone(handlers), wake: make(chan struct{}, 1)}, nil
}

// Start 开始消费队列；已启动时可重入，旧处理器尚未退出时不能再开一组。
func (s *effectScheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.active; r != nil {
		select {
		case <-r.done:
		default:
			if r.ctx.Err() != nil {
				return errors.New("effect 层仍在停止")
			}
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("启动 effect 层: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	s.active = &effectRun{effectScheduler: s, ctx: ctx, cancel: cancel,
		done: make(chan struct{}), finished: make(chan effectCompletion)}
	// 崩溃时正在执行的行仍在库中，启动即按到期时间重放，因此处理器的外部动作必须幂等。
	go s.active.loop()
	return nil
}

// Kick 通知调度器重读已提交的队列，并发通知可合并。
func (s *effectScheduler) Kick() {
	// 合并连续唤醒；写入必须先提交，下一轮读快照才能看见它。
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Stop 取消执行池，等待至处理器全部返回或 ctx 到期；可再次调用以等候退出。
func (s *effectScheduler) Stop(ctx context.Context) error {
	s.mu.Lock()
	r := s.active
	if r != nil {
		r.cancel()
	}
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	select {
	case <-r.done:
		return nil
	default:
	}
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等候 effect 层停止: %w", ctx.Err())
	}
}
