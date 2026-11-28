package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
)

// DaemonConfig 指定一次装载周期。OnHealth 可并发读取 Health，须及时返回。
type DaemonConfig struct {
	StateDir       string
	ExtensionDirs  []string
	Clock          alarm.Clock
	Logger         *slog.Logger
	Version        string
	AlarmLimit     int
	AlarmOverrides map[string]AlarmOverride
	OnHealth       func()
	// OnEvent 在提交后通知本地事件流，只能置位，不得等待客户端。
	OnEvent func()
}

// AlarmOverride 以登记的闹钟 ID 寻址；省略的字段沿用内核或扩展的默认值。
type AlarmOverride struct {
	Every   time.Duration
	Enabled *bool
}

// Daemon 组合单个状态目录中的服务。进程入口负责取得单实例锁并收回本地连接。
// 一个实例只管理一次启停，调用方用 Done 等待指纹变化后的退出。
type Daemon struct {
	initialFingerprint string
	knownExtensions    map[string]bool
	cfg                DaemonConfig
	health             *daemonHealth
	db                 *store.SQLite
	runtime            *Runtime
	executor           *executor
	hosts              map[string]*exthost.Host
	order              []string
	alarms             *alarm.Service
	queue              *alarmQueue
	outbound           *outbound.Service

	mu      sync.Mutex
	started bool
	stop    context.CancelFunc
	done    chan struct{}
	err     error
}

// NewDaemon 不访问文件。配置由调用方在启动前确定，之后不能修改。
func NewDaemon(cfg DaemonConfig) *Daemon {
	if cfg.Clock == nil {
		cfg.Clock = alarm.RealClock{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.AlarmLimit == 0 {
		cfg.AlarmLimit = 4
	}
	return &Daemon{cfg: cfg, health: newDaemonHealth(cfg.OnHealth), hosts: map[string]*exthost.Host{}, done: make(chan struct{})}
}

// Health 返回各来源最近的健康信息，通知回调也可调用。
func (d *Daemon) Health() DaemonHealth { return d.health.snapshot() }

// Done 在停止步骤全部执行完后关闭。随后用 Stop 取回汇总错误。
func (d *Daemon) Done() <-chan struct{} { return d.done }

// Start 等待首轮装载与握手。ctx 控制启动；成功后各组件由 Stop 和指纹监视循环管理。
func (d *Daemon) Start(ctx context.Context) error {
	d.mu.Lock()
	if d.started {
		d.mu.Unlock()
		return fmt.Errorf("daemon 已启动或停止")
	}
	d.started = true
	run, cancel := context.WithCancel(context.Background())
	d.stop = cancel
	d.mu.Unlock()

	// Stop 也能取消尚在握手的启动，不必等它拿着生命周期锁走完所有扩展。
	boot, endBoot := context.WithCancel(ctx)
	unlink := context.AfterFunc(run, endBoot)
	defer func() { unlink(); endBoot() }()
	if err := d.start(boot, run); err != nil {
		cancel()
		d.err = errors.Join(err, d.shutdown())
		close(d.done)
		return d.err
	}
	return nil
}

func (d *Daemon) start(ctx, run context.Context) error {
	if d.cfg.StateDir == "" {
		return fmt.Errorf("缺少状态目录")
	}
	stateDir, err := filepath.Abs(d.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("解析状态目录: %w", err)
	}
	d.cfg.StateDir = stateDir
	if err := os.MkdirAll(d.cfg.StateDir, 0700); err != nil {
		return fmt.Errorf("创建状态目录: %w", err)
	}
	db, err := store.OpenSQLite(ctx, filepath.Join(d.cfg.StateDir, "craft.db"), d.cfg.Logger, store.WithClock(d.cfg.Clock.Now))
	if err != nil {
		return err
	}
	d.db = db
	// 先取装载前的指纹，装载期间变动也会在下一拍被发现。
	fingerprint := d.fingerprint()
	d.initialFingerprint = fingerprint
	result := exthost.Discover(d.cfg.ExtensionDirs...)
	for path, err := range result.Rejected {
		d.health.set(d.health.state.Rejected, path, "rejected", err.Error())
	}
	extensions, err := d.selected(ctx, result.Extensions)
	if err != nil {
		return err
	}
	extensions = d.compatible(extensions)
	if err := d.startRuntime(ctx, extensions); err != nil {
		return err
	}
	for _, ext := range extensions {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := d.startHost(ctx, ext); err != nil {
			return err
		}
	}
	// 只有取得单实例锁且所有扩展完成首轮握手后，才能核实上一代的发送。
	if d.Health().Outbox.Executing > 0 {
		if err := d.outbound.Recover(ctx); err != nil {
			if ctx.Err() != nil || d.Health().Outbox.Executing != 0 {
				return fmt.Errorf("恢复外发记录: %w", err)
			}
			d.cfg.Logger.Warn("部分外发结果无法核实", "error", err)
		}
	}
	if err := d.startAlarms(ctx, extensions); err != nil {
		return err
	}
	timer := d.cfg.Clock.NewTimer(passPeriod)
	go d.monitor(run, timer, fingerprint)
	return nil
}

func (d *Daemon) startRuntime(ctx context.Context, extensions []exthost.Extension) error {
	scope, catalog := daemonCatalog(extensions)
	settings, err := loadDaemonSettings(ctx, d.db)
	if err != nil {
		return err
	}
	d.runtime, err = NewRuntime(d.db, d.cfg.Clock, scope, nil, nil)
	if err != nil {
		return err
	}
	if err := d.setupExecutor(); err != nil {
		return err
	}
	if err := d.startOutbound(ctx, extensions); err != nil {
		return err
	}
	return d.startEffects(ctx, scope, settings, catalog)
}

// 恢复、启动调度、再开始 Pass。测试可在同一入口接入离线驱动。
func (d *Daemon) startEffects(ctx context.Context, scope engine.SourceScope, settings engine.Settings, catalog []engine.EventDef) error {
	effects, err := NewEffectScheduler(d.db, d.cfg.Clock, 2*time.Second, 5*time.Minute, d.health.effect, assessmentHandler{d.runtime, scope}, executorHandler{d.executor, "launch"}, executorHandler{d.executor, "delivery"}, executorHandler{d.executor, "stop"})
	if err != nil {
		return err
	}
	d.runtime.effects = effects
	d.runtime.repo.onEvent = d.cfg.OnEvent
	// 启动调用者可在成功后释放 ctx。执行池一直保留到 Pass 退出之后。
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := d.runtime.start(context.WithoutCancel(ctx), settings, catalog, func() error { return d.executor.sample(ctx, true) }); err != nil {
		return err
	}
	return d.observeEffects(ctx)
}

func (d *Daemon) compatible(exts []exthost.Extension) []exthost.Extension {
	var loaded []exthost.Extension
	available := map[string]bool{}
	for _, e := range exts {
		id := e.Manifest.ID
		if e.Manifest.Protocol != 1 {
			d.health.set(d.health.state.Extensions, id, "incompatible", fmt.Sprintf("不支持协议主版本 %d", e.Manifest.Protocol))
			continue
		}
		if i := slices.IndexFunc(e.Manifest.Requires, func(dep string) bool { return !available[dep] }); i >= 0 {
			d.health.set(d.health.state.Rejected, e.ManifestPath, "rejected", "依赖不可用 "+e.Manifest.Requires[i])
			continue
		}
		available[id] = true
		loaded = append(loaded, e)
	}
	return loaded
}

type restartClock struct{ alarm.Clock }

// After 让扩展的重启退避和闹钟使用同一条时间线。
func (c restartClock) After(d time.Duration) <-chan time.Time { return c.NewTimer(d).C }

func (d *Daemon) startHost(ctx context.Context, ext exthost.Extension) error {
	for _, dep := range ext.Manifest.Requires {
		if d.hosts[dep] == nil {
			d.health.set(d.health.state.Rejected, ext.ManifestPath, "rejected", "依赖未就绪 "+dep)
			return nil
		}
	}
	handler, err := newExtensionHandler(ext.Manifest, d.runtime, d.health)
	if err != nil {
		return err
	}
	id := ext.Manifest.ID
	stateDir := filepath.Join(d.cfg.StateDir, "state", id)
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		d.health.set(d.health.state.Rejected, ext.ManifestPath, "rejected", err.Error())
		return nil
	}
	h, err := exthost.Start(ctx, ext, exthost.Options{KernelVersion: d.cfg.Version, Config: []byte("{}"), StateDir: stateDir, LogDir: filepath.Join(d.cfg.StateDir, "logs"), Clock: restartClock{d.cfg.Clock}, Handler: handler, OnHealth: func(id, state, detail string) {
		status := "ready"
		if state != "ok" {
			status = "restarting"
		}
		d.health.set(d.health.state.Extensions, id, status, detail)
	}})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.health.set(d.health.state.Extensions, id, "rejected", err.Error())
		return nil
	}
	d.hosts[id] = h
	d.order = append(d.order, id)
	return nil
}

func (d *Daemon) startAlarms(ctx context.Context, exts []exthost.Extension) error {
	// 一次超时即可永久占住名额，后续只有 skipped；首次失败就要进入健康快照。
	s, err := alarm.NewService(alarm.Config{Clock: d.cfg.Clock, GlobalLimit: d.cfg.AlarmLimit, FailureThreshold: 1, OnHealth: func(id string, bad bool) {
		status, detail := "ok", ""
		if bad {
			status = "error"
			for _, snap := range d.alarms.List() {
				if snap.ID == id {
					detail = snap.LastError
					break
				}
			}
		}
		d.health.set(d.health.state.Alarms, id, status, detail)
	}})
	if err != nil {
		return err
	}
	d.alarms = s
	d.queue = newAlarmQueue(d.db, s)
	if err := s.Register(alarm.AlarmSpec{ID: "watch.sessions", Every: 10 * time.Second, Group: "watch", Run: d.executor.monitor}); err != nil {
		return err
	}
	override := d.cfg.AlarmOverrides["watch.prune"]
	prune := alarm.AlarmSpec{ID: "watch.prune", Group: "watch", At: "00:00", Enabled: override.Enabled, Run: d.prune}
	if override.Every != 0 {
		prune.Every, prune.At = override.Every, ""
	}
	if err := s.Register(prune); err != nil {
		return fmt.Errorf("登记保留期清理闹钟: %w", err)
	}
	if err := d.queue.recover(ctx); err != nil {
		return err
	}
	for _, e := range exts {
		h := d.hosts[e.Manifest.ID]
		if h == nil {
			continue
		}
		if n := e.Manifest.AlarmLimits.Concurrency; n != nil {
			if err := s.SetGroupLimit(e.Manifest.ID, *n); err != nil {
				return err
			}
		}
		for _, a := range e.Manifest.Alarms {
			id := e.Manifest.ID + ":" + a.ID
			override := d.cfg.AlarmOverrides[id]
			if override.Enabled != nil {
				a.DefaultEnabled = *override.Enabled
			}
			if override.Every != 0 {
				a.Every, a.At = override.Every, ""
			}
			run := func(ctx context.Context, req alarm.RunRequest) error {
				return h.AlarmRun(ctx, a.ID, req.Reason, req.ScheduledAt.UnixMilli())
			}
			if err := s.Register(alarm.AlarmSpec{ID: id, Every: a.Every, At: a.At, Jitter: a.Jitter, Timeout: a.Timeout, Enabled: &a.DefaultEnabled, Group: e.Manifest.ID, Run: d.queue.wrap(id, run)}); err != nil {
				return err
			}
			d.health.set(d.health.state.Alarms, id, "ok", "")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Start(context.WithoutCancel(ctx))
}

func (d *Daemon) monitor(ctx context.Context, timer *time.Timer, fingerprint string) {
	defer func() {
		timer.Stop()
		d.stop()
		d.err = d.shutdown()
		close(d.done)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		timer = d.cfg.Clock.NewTimer(passPeriod)
		if d.fingerprintChanged(fingerprint) {
			return
		}
		if err := d.queue.poll(ctx); err != nil && ctx.Err() == nil {
			d.cfg.Logger.Error("消费闹钟请求失败", "error", err)
		}
		if err := d.observeEffects(ctx); err != nil && ctx.Err() == nil {
			d.cfg.Logger.Error("读取未启用的 effect 队列失败", "error", err)
		}
	}
}

// Stop 可由多个调用方等候。等待方超时不打断清理，后续调用可取回最终错误。
func (d *Daemon) Stop(ctx context.Context) error {
	d.mu.Lock()
	if !d.started {
		d.started = true
		close(d.done)
	}
	if d.stop != nil {
		d.stop()
	}
	d.mu.Unlock()
	select {
	case <-d.done:
		return d.err
	default:
	}
	select {
	case <-d.done:
		return d.err
	case <-ctx.Done():
		return fmt.Errorf("等待 daemon 停止: %w", ctx.Err())
	}
}

func (d *Daemon) shutdown() error {
	ctx := context.Background()
	var errs []error
	if d.alarms != nil {
		// 失去响应的远端仍占闹钟名额。宽限期到后继续停止 Pass 和 Host，以断连释放它。
		grace, cancel := context.WithTimeout(ctx, 3*time.Second)
		errs = append(errs, d.alarms.Stop(grace))
		cancel()
	}
	if d.runtime != nil {
		errs = append(errs, d.runtime.stopPass(ctx))
		errs = append(errs, d.runtime.effects.Stop(ctx))
	}
	for _, id := range slices.Backward(d.order) {
		if err := d.hosts[id].Stop(); err != nil {
			errs = append(errs, fmt.Errorf("停止扩展 %q: %w", id, err))
		}
		d.health.set(d.health.state.Extensions, id, "stopped", "")
	}
	if d.alarms != nil {
		errs = append(errs, d.alarms.Stop(ctx))
		errs = append(errs, d.queue.flush(ctx))
	}
	if d.executor != nil {
		errs = append(errs, d.executor.close())
	}
	if d.db != nil {
		errs = append(errs, d.db.Close())
	}
	return errors.Join(errs...)
}
