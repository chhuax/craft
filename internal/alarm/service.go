package alarm

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"
)

// Service 管理一次启停周期。须用 NewService 创建，各方法可并发调用，使用后不可复制。
type Service struct {
	mu      sync.Mutex
	cfg     Config
	alarms  map[string]*entry
	limits  map[string]int
	groups  map[string]int
	active  int
	ctx     context.Context
	cancel  context.CancelFunc
	stopped bool
	wake    chan struct{}
	done    chan struct{}
}

type entry struct {
	spec    AlarmSpec
	snap    Snapshot
	at      time.Time
	nominal time.Time // 保留无抖动的节拍，防止每一拍的偏移累加。
	enabled bool
	startup bool
	removed bool
	cancel  context.CancelCauseFunc // Run 实际返回前保留名额，包括已超时的运行。
}

// NewService 创建尚未启动的服务，可并发调用。GlobalLimit 必须为正，随机源由服务独占。
func NewService(cfg Config) (*Service, error) {
	if cfg.GlobalLimit < 1 || cfg.FailureThreshold < 0 {
		return nil, fmt.Errorf("闹钟总并发上限须为正，健康阈值不能为负")
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock{}
	}
	if cfg.FailureThreshold == 0 {
		cfg.FailureThreshold = 5
	}
	return &Service{
		cfg:    cfg,
		alarms: make(map[string]*entry),
		limits: make(map[string]int),
		groups: make(map[string]int),
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
	}, nil
}

func (s *Service) stopping() bool {
	return s.stopped || (s.ctx != nil && s.ctx.Err() != nil)
}

// signal 合并同时发生的变更；调度循环每次重读全部计划，只需要一个唤醒信号。
func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Register 可在 Start 前后并发登记。ID 须唯一，返回后调用方可复用声明。
func (s *Service) Register(spec AlarmSpec) error {
	at, err := spec.validate()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping() {
		return fmt.Errorf("登记闹钟 %q: 服务已停止", spec.ID)
	}
	if _, ok := s.alarms[spec.ID]; ok {
		return fmt.Errorf("登记闹钟 %q: ID 已占用", spec.ID)
	}
	enabled := spec.Enabled == nil || *spec.Enabled
	spec.Enabled = nil // 用户可在登记后修改原有开关；服务只保留当时的值。
	e := &entry{
		spec: spec, at: at, enabled: enabled, startup: spec.Startup,
		snap: Snapshot{ID: spec.ID},
	}
	s.alarms[spec.ID] = e
	if s.ctx != nil {
		s.planFirst(e, s.cfg.Clock.Now())
		s.signal()
	}
	return nil
}

// Unregister 可并发调用，立即撤下计划并取消运行。旧回调返回后才可复用同一 ID。
func (s *Service) Unregister(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.alarms[id]
	if e == nil || e.removed {
		return fmt.Errorf("注销闹钟 %q: 未登记", id)
	}
	e.removed = true
	if e.cancel != nil {
		e.cancel(context.Canceled)
	} else {
		delete(s.alarms, id)
	}
	s.signal()
	return nil
}

// SetGroupLimit 可在启动前或运行中并发设置正整数上限。调低只影响后续触发。
func (s *Service) SetGroupLimit(group string, n int) error {
	if n < 1 {
		return fmt.Errorf("闹钟组 %q 的并发上限 %d 须为正", group, n)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping() {
		return fmt.Errorf("设置闹钟组 %q: 服务已停止", group)
	}
	s.limits[group] = n
	return nil
}

// Start 可并发、重复调用。首次启动的 ctx 管理整个服务，取消后与 Stop 一样终止调度。
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping() {
		return fmt.Errorf("启动闹钟: 服务已停止")
	}
	if s.ctx != nil {
		return nil
	}
	if ctx == nil {
		return fmt.Errorf("启动闹钟: ctx 为空")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("启动闹钟: %w", err)
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	now := s.cfg.Clock.Now()
	// 按 ID 分配首次抖动，使同一随机种子不受 map 遍历顺序影响。
	for _, id := range slices.Sorted(maps.Keys(s.alarms)) {
		s.planFirst(s.alarms[id], now)
	}
	go s.loop()
	return nil
}

// Stop 可并发、重复调用。先关闭触发入口并取消所有运行，再等到回调返回或 ctx 结束。
// 回调内不能同步等待自身结束；等待超时后可用新的 ctx 再次等待。
func (s *Service) Stop(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("停止闹钟: ctx 为空")
	}
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		if s.cancel != nil {
			s.cancel()
		} else {
			close(s.done)
		}
	}
	s.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		select {
		case <-s.done:
			return nil
		default:
			return fmt.Errorf("等待闹钟停止: %w", ctx.Err())
		}
	}
}

// Trigger 可并发请求已启动服务中的闹钟，接纳后立即返回。无名额时记 skipped 并返回错误。
func (s *Service) Trigger(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.stopping() {
		return fmt.Errorf("触发闹钟 %q: 服务未运行", id)
	}
	e := s.alarms[id]
	if e == nil || e.removed {
		return fmt.Errorf("触发闹钟 %q: 未登记", id)
	}
	if !s.admit(e, "requested", s.cfg.Clock.Now()) {
		return fmt.Errorf("触发闹钟 %q: 已跳过，运行名额被占用", id)
	}
	return nil
}

// List 可并发调用，按 ID 返回独立的快照。已取消但尚未返回的 Run 仍占用名额。
func (s *Service) List() []Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := make([]Snapshot, 0, len(s.alarms))
	for _, e := range s.alarms {
		if e.removed {
			continue
		}
		snap := e.snap
		switch {
		case s.stopping():
			snap.State, snap.Next = "stopped", time.Time{}
		case e.cancel != nil:
			snap.State = "running"
		case !e.enabled:
			snap.State = "disabled"
		case snap.ConsecutiveFailures != 0:
			snap.State = "backoff"
		default:
			snap.State = "idle"
		}
		list = append(list, snap)
	}
	slices.SortFunc(list, func(a, b Snapshot) int { return cmp.Compare(a.ID, b.ID) })
	return list
}
