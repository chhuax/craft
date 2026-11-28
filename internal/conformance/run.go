package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

// Options 的零值使用扩展目录下的默认 fixture、系统时钟和有限的阶段截止时间。
type Options struct {
	Fixtures             string
	KernelVersion        string
	Now                  func() time.Time
	Timeout, CancelGrace time.Duration
}

// Check 是一条独立结论，Detail 给出失败调用或缺少的覆盖条件。
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

const (
	manifest = iota
	handshake
	httpReplay
	alarms
	cursor
	factOrder
	sources
	determinism
	facts
	identity
	mapping
	cancellation
	strict
)

type report []Check

func newReport() report {
	names := []string{"manifest", "initialize", "http", "alarm/run", "§5.1 游标", "§5.2 状态事实", "§5.3 来源", "§5.4 事件标识", "§5.5 facts", "§5.6 对象标识", "§5.7 错误映射", "§5.8 取消", "§5.9 严格 JSON"}
	r := make(report, len(names))
	for i, n := range names {
		r[i] = Check{n, "skip", "前置检查未完成"}
	}
	return r
}

func (r report) pass(i int) {
	if r[i].Status != "fail" {
		r[i].Status = "pass"
		r[i].Detail = ""
	}
}
func (r report) skip(i int, why string) {
	if r[i].Status != "fail" {
		r[i].Status = "skip"
		r[i].Detail = why
	}
}
func (r report) fail(i int, format string, args ...any) {
	if r[i].Status != "fail" {
		r[i].Status = "fail"
		r[i].Detail = fmt.Sprintf(format, args...)
	}
}

type suite struct {
	rounds [][]trial
	Options
	r      report
	ext    exthost.Extension
	scope  exthost.SourceScope
	trace  transcript
	replay *replay
	config json.RawMessage
	temp   string
	now    time.Time
}

func discover(dir string) (exthost.Extension, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return exthost.Extension{}, err
	}
	root := filepath.Dir(abs)
	found := exthost.Discover(root)
	for _, e := range found.Extensions {
		if e.Dir == abs {
			return e, nil
		}
	}
	if err := found.Rejected[filepath.Join(abs, "manifest.json")]; err != nil {
		return exthost.Extension{}, err
	}
	if err := found.Rejected[root]; err != nil {
		return exthost.Extension{}, err
	}
	return exthost.Extension{}, fmt.Errorf("扩展目录 %s 缺少可装载的 manifest.json", abs)
}

// Run 在独立的临时状态目录启动指定扩展；返回前回收全部子进程与回放监听器。
func Run(ctx context.Context, dir string, opt Options) []Check {
	if opt.Now == nil {
		opt.Now = time.Now
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 10 * time.Second
	}
	if opt.CancelGrace <= 0 {
		opt.CancelGrace = time.Second
	}
	if opt.KernelVersion == "" {
		opt.KernelVersion = "conformance"
	}
	s := suite{Options: opt, r: newReport(), now: opt.Now()}
	ext, err := discover(dir)
	if err != nil {
		s.r.fail(manifest, "%v", err)
		return s.r
	}
	s.ext = ext
	s.scope = exthost.NewSourceScope(ext.Manifest)
	s.r.pass(manifest)
	if opt.Fixtures == "" {
		opt.Fixtures = filepath.Join(ext.Dir, "testdata", "conformance")
	}
	f, err := loadFixture(opt.Fixtures)
	if err != nil {
		s.r.fail(httpReplay, "读取 fixture: %v", err)
		return s.r
	}
	s.temp, err = os.MkdirTemp("", "craft-conformance-*")
	if err != nil {
		s.r.fail(handshake, "建立临时状态目录: %v", err)
		return s.r
	}
	defer func() {
		if err := os.RemoveAll(s.temp); err != nil {
			s.r.fail(handshake, "回收临时状态: %v", err)
		}
	}()
	base := ""
	if f.provided {
		s.replay, err = startReplay(f, &s.trace, opt.Timeout)
		if err != nil {
			s.r.fail(httpReplay, "启动回放服务器: %v", err)
			return s.r
		}
		defer func() {
			if err := s.replay.close(); err != nil {
				s.r.fail(httpReplay, "回收 HTTP 监听器: %v", err)
			}
		}()
		base = s.replay.base
	}
	s.config, err = f.configuration(base)
	if err != nil {
		s.r.fail(httpReplay, "替换 config.json: %v", err)
		return s.r
	}
	if s.replay == nil {
		for i := httpReplay; i < len(s.r); i++ {
			s.r.skip(i, "未提供 fixture，行为类检查已跳过")
		}
	} else {
		for i := httpReplay; i < len(s.r); i++ {
			s.r.skip(i, "fixture 未覆盖该场景")
		}
		defer s.inspectTransport()
	}
	s.run(ctx)
	return s.r
}

type session struct {
	conn   *exthost.Conn
	k      *kernel
	finish func(context.Context) error
}

func (s *suite) start(label string) (*session, string, error) {
	dir, err := os.MkdirTemp(s.temp, "state-")
	if err != nil {
		return nil, "", err
	}
	k := newKernel(s.scope, s.now)
	s.trace.begin(label)
	conn, finish, err := exthost.OpenProcess(s.ext, s.temp, k.handle, s.trace.wire)
	if err != nil {
		return nil, "", err
	}
	return &session{conn, k, finish}, dir, nil
}

func (s *suite) initialize(ctx context.Context, p *session, state string, major int) error {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	params := struct {
		protocol.InitializeParams
		Future bool `json:"futureField"`
	}{protocol.InitializeParams{Protocol: major, KernelVersion: s.KernelVersion, Config: s.config, StateDir: state}, true}
	var raw json.RawMessage
	err := p.conn.Call(ctx, "initialize", params, &raw)
	if err != nil {
		if _, ok := errors.AsType[*protocol.Error](err); major != 1 && ok {
			return nil
		}
		return err
	}
	var result struct {
		OK           *bool           `json:"ok"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return err
	}
	if result.OK == nil {
		return errors.New("initialize 缺少布尔字段 ok")
	}
	if major != 1 {
		if *result.OK {
			return fmt.Errorf("期望拒绝 protocol %d", major)
		}
		return nil
	}
	if !*result.OK || len(result.Capabilities) == 0 || bytes.Equal(result.Capabilities, []byte("null")) {
		return errors.New("期望 ok=true 并返回 capabilities")
	}
	return nil
}

func (s *suite) stop(ctx context.Context, p *session, check int, graceful bool) {
	var grace context.Context
	if graceful {
		ctx, cancel := context.WithTimeout(ctx, s.Timeout)
		defer cancel()
		grace = ctx
		err := p.conn.Call(ctx, "shutdown", struct{}{}, nil)
		s.checkFrame("shutdown", err)
		if err != nil && !errors.Is(err, exthost.ErrUnavailable) {
			s.r.fail(check, "shutdown: %v", err)
		}
	}
	if err := p.finish(grace); err != nil {
		s.r.fail(check, "回收子进程树: %v", err)
	}
}

func (s *suite) open(ctx context.Context, label string, major int) *session {
	p, dir, err := s.start(label)
	if err == nil {
		err = s.initialize(ctx, p, dir, major)
		s.checkFrame(label, err)
		if err != nil {
			s.stop(ctx, p, handshake, false)
		}
	}
	if err != nil {
		s.r.fail(handshake, "%s initialize: %v", label, err)
		return nil
	}
	return p
}

func (s *suite) run(ctx context.Context) {
	if s.ext.Manifest.Protocol != 1 {
		s.r.fail(handshake, "manifest protocol=%d，期望主版本 1", s.ext.Manifest.Protocol)
		return
	}
	// 两种主版本用不同进程；已 ready 的扩展按生命周期可以拒绝所有重复 initialize。
	reject := s.open(ctx, "不兼容版本", 2)
	if reject == nil {
		return
	}
	s.stop(ctx, reject, handshake, false)
	p := s.open(ctx, "首次启动", 1)
	if p == nil {
		return
	}
	s.r.pass(handshake)
	if s.replay == nil || len(s.ext.Manifest.Alarms) == 0 {
		s.stop(ctx, p, handshake, true)
		return
	}
	for _, reason := range []string{"startup", "requested"} {
		runs, ok := s.round(ctx, p, reason, len(s.rounds)+1)
		s.rounds = append(s.rounds, runs)
		if !ok {
			s.stop(ctx, p, alarms, false)
			return
		}
	}
	// 热进程已留下游标。取消探针结束（或强制回收）后再冷启动，两项结论互不污染。
	s.probeCancel(ctx, p, s.rounds[1])
	s.stop(ctx, p, alarms, s.r[cancellation].Status != "fail")
	cold := s.open(ctx, "冷启动", 1)
	if cold == nil {
		return
	}
	runs, ok := s.round(ctx, cold, "startup", 3)
	s.rounds = append(s.rounds, runs)
	s.stop(ctx, cold, alarms, ok)
}

type trial struct {
	label, alarm string
	events       []event
	err          error
}

func (s *suite) round(ctx context.Context, p *session, reason string, n int) ([]trial, bool) {
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()
	s.replay.reset()
	// 热重放只清 inbox。扩展进程、私有 KV 和磁盘游标继续使用上一轮状态。
	p.k.mu.Lock()
	clear(p.k.inbox)
	p.k.mu.Unlock()
	var runs []trial
	for _, a := range s.ext.Manifest.Alarms {
		label := fmt.Sprintf("第 %d 轮 alarm/run %s", n, a.ID)
		pos := s.trace.begin(label)
		err := p.conn.Call(ctx, "alarm/run", protocol.AlarmRunParams{AlarmID: a.ID, Reason: reason, ScheduledAtMs: s.now.UnixMilli()}, nil)
		s.checkFrame(label, err)
		runs = append(runs, trial{label, a.ID, s.trace.since(pos), err})
		if _, remote := errors.AsType[*protocol.Error](err); err != nil && !remote {
			s.r.fail(alarms, "%s 应在截止前返回 RPC 响应: %v", label, err)
			return runs, false
		}
	}
	s.r.pass(alarms)
	return runs, true
}

func (s *suite) probeCancel(ctx context.Context, p *session, runs []trial) {
	ctx, stop := context.WithTimeout(ctx, s.Timeout)
	defer stop()
	for _, run := range runs {
		if run.err != nil {
			continue
		}
		for _, e := range run.events {
			if e.httpStatus >= 200 && e.httpStatus < 300 && e.detail == "" {
				if s.cancelAlarm(ctx, p, run) {
					return
				}
				break
			}
		}
	}
}

func (s *suite) cancelAlarm(ctx context.Context, p *session, run trial) bool {
	id := run.alarm
	gate := &httpGate{entered: make(chan struct{}), release: make(chan struct{})}
	s.replay.reset()
	s.replay.mu.Lock()
	s.replay.gate = gate
	s.replay.mu.Unlock()
	defer func() { s.replay.mu.Lock(); s.replay.gate = nil; s.replay.mu.Unlock() }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()
	pos := s.trace.begin("取消 alarm/run " + id)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- p.conn.CallWaiting(runCtx, "alarm/run", protocol.AlarmRunParams{AlarmID: id, Reason: "requested", ScheduledAtMs: s.now.UnixMilli()}, nil)
	}()
	select {
	case <-gate.entered:
	case err := <-done:
		s.checkFrame("取消 alarm/run "+id, err)
		if err != nil {
			s.r.fail(cancellation, "alarm/run %s 在进入重放场景之前出错: %v", id, err)
			return true
		}
		return false
	case <-ctx.Done():
		s.r.fail(cancellation, "alarm/run %s 未在截止前进入重放场景: %v", id, ctx.Err())
		return true
	}
	// 收到上游请求证明远端处理已开始。取消通知发出后放行 HTTP，允许只在分页边界检查取消的扩展退出。
	s.trace.mu.Lock()
	s.trace.onCancel = release
	s.trace.mu.Unlock()
	cancel()
	grace, stop := context.WithTimeout(ctx, s.CancelGrace)
	defer stop()
	select {
	case err := <-done:
		s.checkFrame("取消 alarm/run "+id, err)
		if errors.Is(err, context.Canceled) {
			if canceledWork(run.events, s.trace.since(pos)) {
				s.r.pass(cancellation)
			} else {
				s.r.skip(cancellation, "宽限期内正常返回，但与未取消时完成了相同的调用，fixture 未证明循环提前结束")
				return false
			}
		} else {
			s.r.fail(cancellation, "alarm/run %s 取消后期望 RPC 响应，得到 %v", id, err)
		}
	case <-grace.Done():
		s.r.fail(cancellation, "alarm/run %s 收到 $/cancel 后应在 %v 内返回", id, s.CancelGrace)
	}
	return true
}

// 短任务也可能碰巧在宽限期内正常跑完。用相同录制下的正常调用作对照，
// 只有返回错误或少处理了后续工作时，才能把及时返回作为取消的证据。
func canceledWork(baseline, probe []event) bool {
	var alarmID json.RawMessage
	for _, e := range probe {
		if !e.in && e.Method == "alarm/run" {
			alarmID = e.ID
		}
		if e.in && e.Method == "" && e.Error != nil && bytes.Equal(e.ID, alarmID) {
			return true
		}
	}
	work := func(events []event) int {
		n := 0
		for _, e := range events {
			if e.httpStatus != 0 || e.in && e.Method != "" {
				n++
			}
		}
		return n
	}
	return work(probe) < work(baseline)
}

func (s *suite) checkFrame(label string, err error) {
	if s.replay != nil && errors.Is(err, exthost.ErrLineTooLong) {
		s.r.fail(strict, "%s: 期望可完整解析的一行 JSON: %v", label, err)
	}
}

func (s *suite) inspectTransport() {
	events := s.trace.since(0)
	s.inspect(events)
	s.checkWindow(events)
	for _, e := range events {
		switch {
		case e.httpStatus != 0:
			if e.detail != "" {
				s.r.fail(httpReplay, "%s: %s (%d)", e.where(), e.detail, e.httpStatus)
			} else {
				s.r.pass(httpReplay)
			}
		case e.in:
			if e.badJSON {
				s.r.fail(strict, "%s: %s", e.where(), e.detail)
			} else {
				s.r.pass(strict)
			}
		}
	}
}
