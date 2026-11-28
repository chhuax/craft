package alarm

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func service(t *testing.T, cfg Config) (*Service, FakeClock) {
	t.Helper()
	clock := FakeClock{}
	cfg.Clock = clock
	s, err := NewService(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s, clock
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestEveryAndStartup(t *testing.T) {
	for _, startup := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			s, c := service(t, Config{GlobalLimit: 1})
			var calls []RunRequest
			now := c.Now()
			spec := AlarmSpec{ID: "a", Every: time.Minute, Startup: startup, Run: func(_ context.Context, req RunRequest) error {
				calls = append(calls, req)
				return nil
			}}
			must(t, s.Register(spec))
			must(t, s.Start(t.Context()))
			must(t, s.Start(t.Context()))
			c.Advance(3*time.Minute - 1)
			var want []RunRequest
			if startup {
				want = append(want, RunRequest{"a", "startup", now})
			}
			for i := 1; i <= 2; i++ {
				want = append(want, RunRequest{"a", "schedule", now.Add(time.Duration(i) * time.Minute)})
			}
			if !slices.Equal(calls, want) || !s.List()[0].Next.Equal(now.Add(3*time.Minute)) {
				t.Fatalf("启动触发 %v，调用 %v，快照 %+v", startup, calls, s.List())
			}
			must(t, s.Unregister("a"))
			c.Advance(time.Minute)
			if len(s.List()) != 0 || !slices.Equal(calls, want) {
				t.Fatal("注销后仍有调度")
			}
		})
	}
}

func TestFailureBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var health []bool
		cfg := Config{GlobalLimit: 1, OnHealth: func(id string, bad bool) {
			if id != "a" {
				t.Errorf("健康通知的 ID = %q", id)
			}
			health = append(health, bad)
		}}
		s, c := service(t, cfg)
		var n int
		spec := AlarmSpec{ID: "a", Every: time.Hour, Run: func(_ context.Context, req RunRequest) error {
			n++
			if n == 7 {
				if req.Reason != "requested" {
					t.Errorf("退避中触发的原因 = %q", req.Reason)
				}
				return nil
			}
			return errors.New("运行出错")
		}}
		must(t, s.Start(t.Context()))
		must(t, s.Register(spec))
		must(t, s.Trigger("a"))
		delays := []time.Duration{2 * time.Second, 10 * time.Second, time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute}
		for i, delay := range delays {
			c.Advance(0)
			got := s.List()[0]
			if got.ConsecutiveFailures != i+1 || got.LastResult != "failed" || got.LastError == "" || got.State != "backoff" || got.Next.Sub(c.Now()) != delay {
				t.Fatalf("第 %d 次失败 %+v，期望退避 %v", i+1, got, delay)
			}
			if i < len(delays)-1 {
				c.Advance(delay)
			}
		}
		must(t, s.Trigger("a"))
		c.Advance(0)
		got := s.List()[0]
		if got.ConsecutiveFailures != 0 || got.LastResult != "success" || got.LastError != "" || got.Next.Sub(c.Now()) != time.Hour || !slices.Equal(health, []bool{true, false}) {
			t.Fatalf("恢复时快照 %+v，通知 %v", got, health)
		}
	})
}

func TestLocalDaily(t *testing.T) {
	original := time.Local
	time.Local = time.FixedZone("UTC+08", 8*60*60)
	defer func() { time.Local = original }()
	for _, tc := range []struct {
		at     string
		offset time.Duration
	}{
		{"09:00", time.Hour}, {"07:30", 23*time.Hour + 30*time.Minute},
	} {
		synctest.Test(t, func(t *testing.T) {
			s, c := service(t, Config{GlobalLimit: 1})
			start := c.Now()
			var calls []time.Time
			must(t, s.Register(AlarmSpec{ID: "daily", At: tc.at, Run: func(_ context.Context, req RunRequest) error {
				calls = append(calls, req.ScheduledAt)
				return nil
			}}))
			must(t, s.Start(t.Context()))
			want := start.Add(tc.offset)
			if !s.List()[0].Next.Equal(want) {
				t.Fatalf("%s 的首次计划 %v，期望 %v", tc.at, s.List()[0].Next, want)
			}
			c.Advance(tc.offset + 24*time.Hour)
			if len(calls) != 2 || !calls[0].Equal(want) || !calls[1].Equal(want.AddDate(0, 0, 1)) {
				t.Fatalf("%s 的跨天调用 %v", tc.at, calls)
			}
		})
	}
}

func TestJitter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rng := func() *rand.Rand { return rand.New(rand.NewPCG(1, 2)) }
		expected := rng()
		s, c := service(t, Config{GlobalLimit: 1, Rand: rng()})
		const span = 5 * time.Second
		must(t, s.Register(AlarmSpec{ID: "a", Every: time.Minute, Jitter: span, Run: func(context.Context, RunRequest) error { return nil }}))
		must(t, s.Start(t.Context()))
		base := c.Now()
		for range 5 {
			base = base.Add(time.Minute)
			next := s.List()[0].Next
			offset := next.Sub(base)
			if offset < 0 || offset > span || offset != time.Duration(expected.Uint64N(uint64(span)+1)) {
				t.Fatalf("计划 %v 相对 %v 的偏移 %v", next, base, offset)
			}
			c.Advance(next.Sub(c.Now()))
		}
	})
}
