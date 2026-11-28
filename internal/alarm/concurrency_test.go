package alarm

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestConcurrencyLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 3})
		must(t, s.SetGroupLimit("two", 2))
		must(t, s.Start(t.Context()))
		started := make(chan string, 16)
		release := make(chan struct{})
		for _, tc := range []struct{ id, group string }{
			{"a", "one"}, {"b", "one"}, {"c", "two"}, {"d", "two"}, {"e", "two"}, {"f", "other"},
		} {
			must(t, s.Register(AlarmSpec{ID: tc.id, Group: tc.group, Every: time.Minute, Run: func(ctx context.Context, req RunRequest) error {
				started <- req.ID
				select {
				case <-ctx.Done():
				case <-release:
				}
				return nil
			}}))
		}
		for _, tc := range []struct {
			id   string
			skip bool
		}{
			{"c", false}, {"d", false}, {"e", true}, {"a", false}, {"a", true}, {"b", true}, {"f", true},
		} {
			if err := s.Trigger(tc.id); (err != nil) != tc.skip {
				t.Fatalf("Trigger(%s) = %v，期望跳过 %v", tc.id, err, tc.skip)
			}
		}
		c.Advance(time.Minute)
		if len(started) != 3 {
			t.Fatalf("运行了 %d 个回调，期望 3 个", len(started))
		}
		for _, snap := range s.List() {
			if snap.LastResult != "skipped" || snap.ConsecutiveFailures != 0 {
				t.Errorf("周期触发未正确记为跳过 %+v", snap)
			}
		}
		close(release)
		c.Advance(0)
		if len(started) != 3 {
			t.Fatal("名额释放后补跑了跳过的请求")
		}
		must(t, s.Trigger("f"))
		c.Advance(0)
		if len(started) != 4 {
			t.Fatal("名额未归还")
		}
	})
}

func TestDisabledExplicitTrigger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 1})
		var calls []RunRequest
		enabled := false
		must(t, s.Register(AlarmSpec{ID: "off", Every: time.Second, Startup: true, Enabled: &enabled, Run: func(_ context.Context, req RunRequest) error {
			calls = append(calls, req)
			return errors.New("本次失败")
		}}))
		enabled = true
		must(t, s.Start(t.Context()))
		c.Advance(time.Hour)
		now := c.Now()
		must(t, s.Trigger("off"))
		c.Advance(2 * time.Hour)
		got := s.List()[0]
		if !slices.Equal(calls, []RunRequest{{"off", "requested", now}}) || !got.Next.IsZero() || got.State != "disabled" || got.ConsecutiveFailures != 1 {
			t.Fatalf("调用 %v，快照 %+v", calls, got)
		}
	})
}

func TestTimeoutKeepsSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var health []bool
		s, c := service(t, Config{GlobalLimit: 1, FailureThreshold: 1, OnHealth: func(_ string, bad bool) { health = append(health, bad) }})
		release := make(chan struct{})
		canceled := make(chan error, 1)
		calls := 0
		must(t, s.Register(AlarmSpec{ID: "a", Every: time.Hour, Timeout: time.Minute, Run: func(ctx context.Context, _ RunRequest) error {
			calls++
			if calls == 1 {
				<-ctx.Done()
				canceled <- context.Cause(ctx)
				<-release
			}
			return nil
		}}))
		must(t, s.Start(t.Context()))
		must(t, s.Trigger("a"))
		// 即使下面的断言失败，也让不响应取消的回调返回，便于清理服务。
		defer close(release)
		c.Advance(time.Minute - 1)
		if len(canceled) != 0 || s.List()[0].ConsecutiveFailures != 0 {
			t.Fatal("提前取消了回调")
		}
		c.Advance(1)
		got := s.List()[0]
		var cause error
		select {
		case cause = <-canceled:
		default:
			t.Fatal("超时未取消回调")
		}
		if !errors.Is(cause, context.DeadlineExceeded) || got.ConsecutiveFailures != 1 || got.State != "running" || got.LastResult != "failed" || got.Next.Sub(c.Now()) != 2*time.Second || !slices.Equal(health, []bool{true}) {
			t.Fatalf("超时仍在运行时的快照 %+v，取消原因 %v，通知 %v", got, cause, health)
		}
		if err := s.Trigger("a"); err == nil {
			t.Fatal("超时后提前接受了同一闹钟")
		}
		c.Advance(2 * time.Second)
		if s.List()[0].LastResult != "skipped" || calls != 1 {
			t.Fatal("回调返回前重叠运行了")
		}
		release <- struct{}{}
		c.Advance(0)
		if s.List()[0].ConsecutiveFailures != 1 {
			t.Fatal("超时后返回 nil 覆盖了失败")
		}
		must(t, s.Trigger("a"))
		c.Advance(0)
		if got := s.List()[0]; got.LastResult != "success" || got.ConsecutiveFailures != 0 || !slices.Equal(health, []bool{true, false}) {
			t.Fatalf("超时恢复 %+v，通知 %v", got, health)
		}
	})
}

func TestPanicAndRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 1})
		var calls int
		must(t, s.Register(AlarmSpec{ID: "p", Every: time.Hour, Run: func(context.Context, RunRequest) error {
			calls++
			if calls == 1 {
				panic("回调异常")
			}
			return nil
		}}))
		must(t, s.Start(t.Context()))
		must(t, s.Trigger("p"))
		c.Advance(0)
		if got := s.List()[0]; got.LastResult != "failed" || !strings.Contains(got.LastError, "回调异常") || got.ConsecutiveFailures != 1 {
			t.Fatalf("panic 后快照 %+v", got)
		}
		c.Advance(2 * time.Second)
		if got := s.List()[0]; got.ConsecutiveFailures != 0 || got.LastResult != "success" {
			t.Fatalf("panic 后重试 %+v", got)
		}
	})
}

func TestStopWaitsAndStaysStopped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 1})
		must(t, s.Start(t.Context()))
		release := make(chan struct{})
		defer close(release)
		canceled := make(chan struct{})
		calls := 0
		must(t, s.Register(AlarmSpec{ID: "a", Every: time.Second, Startup: true, Run: func(ctx context.Context, _ RunRequest) error {
			calls++
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		}}))
		c.Advance(0)
		wait, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		first := make(chan error, 1)
		go func() { first <- s.Stop(wait) }()
		c.Advance(0)
		select {
		case <-canceled:
		default:
			t.Fatal("Stop 未取消回调")
		}
		select {
		case err := <-first:
			t.Fatalf("Stop 提前返回 %v", err)
		default:
		}
		c.Advance(time.Minute)
		if err := <-first; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Stop 等待超时 = %v", err)
		}
		if s.Trigger("a") == nil || s.Start(t.Context()) == nil {
			t.Fatal("已停止的服务仍接受触发或启动")
		}
		release <- struct{}{}
		must(t, s.Stop(context.Background()))
		must(t, s.Stop(context.Background()))
		c.Advance(time.Hour)
		if calls != 1 || s.List()[0].State != "stopped" || !s.List()[0].Next.IsZero() {
			t.Fatalf("停止后调用 %d 次，快照 %+v", calls, s.List())
		}
	})
}

func TestUnregisterRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 2})
		release := make(chan struct{})
		defer close(release)
		spec := AlarmSpec{ID: "a", Every: time.Hour, Run: func(ctx context.Context, _ RunRequest) error {
			<-ctx.Done()
			<-release
			return ctx.Err()
		}}
		must(t, s.Start(t.Context()))
		must(t, s.Register(spec))
		must(t, s.Trigger("a"))
		c.Advance(0)
		must(t, s.Unregister("a"))
		if len(s.List()) != 0 || s.Trigger("a") == nil || s.Register(spec) == nil {
			t.Fatal("注销未撤下计划或提前复用了 ID")
		}
		release <- struct{}{}
		c.Advance(0)
		must(t, s.Register(spec))
		if got := s.List()[0]; got.ConsecutiveFailures != 0 || got.LastError != "" || got.State != "idle" {
			t.Fatalf("新登记继承了旧结果 %+v", got)
		}
	})
}

func TestConcurrentTrigger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 4})
		must(t, s.Start(t.Context()))
		calls := make(chan string, 8)
		for _, id := range []string{"a", "b"} {
			must(t, s.Register(AlarmSpec{ID: id, Every: time.Hour, Run: func(ctx context.Context, req RunRequest) error {
				calls <- req.ID
				<-ctx.Done()
				return nil
			}}))
		}
		accepted := make(chan string, 8)
		for _, id := range []string{"a", "b", "b", "a", "a", "b", "b", "a"} {
			go func() {
				if s.Trigger(id) == nil {
					accepted <- id
				}
			}()
		}
		c.Advance(0)
		if len(accepted) != 1 || len(calls) != 1 || <-calls != <-accepted {
			t.Fatal("同组同时触发时应当只有一个请求占用默认名额")
		}
	})
}

func TestLifecycleBoundaries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, c := service(t, Config{GlobalLimit: 1})
		called := false
		spec := AlarmSpec{ID: "a", Every: time.Second, Run: func(context.Context, RunRequest) error {
			called = true
			return nil
		}}
		must(t, s.Register(spec))
		if s.Trigger("a") == nil || s.Register(spec) == nil || s.Unregister("absent") == nil {
			t.Fatal("启动前触发、重复登记与注销未知 ID 应返回错误")
		}
		ctx, cancel := context.WithCancel(t.Context())
		must(t, s.Start(ctx))
		cancel()
		c.Advance(time.Hour)
		must(t, s.Stop(context.Background()))
		if s.Trigger("a") == nil || called {
			t.Fatal("父上下文取消后仍能触发")
		}
		s, _ = service(t, Config{GlobalLimit: 1})
		must(t, s.Stop(context.Background()))
		must(t, s.Stop(context.Background()))
		if s.Start(t.Context()) == nil {
			t.Fatal("先停止的服务又被启动")
		}
	})
}
