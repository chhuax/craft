package app

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

// noopEffects 是没有注册任何处理器时的 EffectLayer（接口见 effects_scheduler.go）。
type noopEffects struct{}

func (noopEffects) Start(context.Context) error { return nil }
func (noopEffects) Kick()                       {}
func (noopEffects) Stop(context.Context) error  { return nil }

// Runtime 管理一个文件库的一次启停周期。Start 成功后通过 Commands 提交本进程命令。
type Runtime struct {
	Commands   *engine.Engine
	pass       *engine.Engine
	repo       *engineStore
	policy     atomic.Pointer[engine.Policy]
	clock      alarm.Clock
	effects    EffectLayer
	stopAlarms func(context.Context) error

	wake             chan struct{}
	mu               sync.Mutex
	cancel           context.CancelFunc
	done             chan struct{}
	stopping, closed bool
}

type commandStore struct {
	*engineStore
	wake func()
}

func (s commandStore) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return s.engineStore.Atomic(ctx, func(u engine.Unit) error {
		u.AfterCommit(s.wake)
		return fn(u)
	})
}

// NewRuntime 接管 db，时钟和进程内的 Scope 为必填依赖；可省略闹钟停止函数和 effect 层。
// Scope 只读本地声明，不能在引擎调用中访问扩展。Stop 之前调用方须收回各命令入口。
func NewRuntime(db *store.SQLite, clock alarm.Clock, scope engine.SourceScope, effects EffectLayer, stopAlarms func(context.Context) error) (*Runtime, error) {
	if db == nil || clock == nil || scope == nil {
		return nil, fmt.Errorf("运行层需要数据库、时钟与来源范围")
	}
	if effects == nil {
		effects = noopEffects{}
	}
	r := &Runtime{repo: &engineStore{db: db}, clock: clock, effects: effects, stopAlarms: stopAlarms, wake: make(chan struct{}, 1)}
	// 首次生效时 PolicyChanged 尚无旧快照；引擎依赖始终可读。
	r.policy.Store(&engine.Policy{})
	var err error
	r.pass, err = r.newEngine(r.repo, scope)
	if err != nil {
		return nil, err
	}
	// Pass 内部的 EvaluateDue 即使暂停也可能提交空事务；只有外部命令需要借提交再次唤醒。
	r.Commands, err = r.newEngine(commandStore{r.repo, r.Request}, scope)
	if err != nil {
		return nil, err
	}
	return r, nil
}

const utcMillis = "2006-01-02T15:04:05.000Z"

func (r *Runtime) newEngine(repo engine.Store, scope engine.SourceScope) (*engine.Engine, error) {
	return engine.New(engine.Config{
		Store: repo, Scope: scope,
		Clock:  func() string { return r.clock.Now().UTC().Format(utcMillis) },
		Policy: func() engine.Policy { return *r.policy.Load() },
	})
}

// Request 合并多个唤醒请求；只有 Pass 循环可以消费它们。
func (r *Runtime) Request() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Start 比对持久化的旧策略后启动 effect 和唯一的 Pass。成功后须显式 Stop 以保证停止顺序。
func (r *Runtime) Start(ctx context.Context, settings engine.Settings, catalog []engine.EventDef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopping {
		return fmt.Errorf("运行层已经停止")
	}
	if r.done != nil {
		return nil
	}
	if err := r.ApplyPolicy(ctx, settings, catalog); err != nil {
		return err
	}
	version, err := r.repo.db.DataVersion(ctx)
	if err != nil {
		return err
	}
	if err := r.effects.Start(ctx); err != nil {
		return fmt.Errorf("启动 effect: %w", err)
	}
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	r.cancel = cancel
	r.done = make(chan struct{})
	r.Request()
	go r.loop(run, version)
	return nil
}

// Stop 按闹钟、Pass、effect、数据库的顺序停止。等待超时可用新的 ctx 继续停止。
func (r *Runtime) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.stopping = true
	if r.stopAlarms != nil {
		if err := r.stopAlarms(ctx); err != nil {
			return fmt.Errorf("停止闹钟: %w", err)
		}
		r.stopAlarms = nil
	}
	if r.done != nil {
		if err := r.stopPass(ctx); err != nil {
			return err
		}
		if err := r.effects.Stop(ctx); err != nil {
			return fmt.Errorf("停止 effect: %w", err)
		}
		r.done = nil
	}
	if err := r.repo.db.Close(); err != nil {
		return err
	}
	r.closed = true
	return nil
}

// daemon 还要在关库前回收扩展。拆出这一步，让两种入口遵循同一条 Pass 停止边界。
// 调用方串行管理生命周期，done 在循环退出前不可替换。
func (r *Runtime) stopPass(ctx context.Context) error {
	if r.done == nil {
		return nil
	}
	r.cancel()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("等待 Pass 结束: %w", ctx.Err())
	}
}
