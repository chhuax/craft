package fake

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/host"
)

func TestDeliveryAndLoss(t *testing.T) {
	d := New(host.Capabilities{}, nil)
	s, err := d.Create(t.Context(), host.CreateSpec{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id   string
		want host.Outcome
	}{
		{"bad/id", host.Unsupported}, {"first", host.OK}, {"first", host.OK}, {"second", host.Later},
	} {
		if r := d.Append(t.Context(), s, "正文", tc.id); r.Outcome != tc.want {
			t.Fatal(r)
		}
	}
	calls := d.Calls()
	if len(calls) != 4 || !strings.HasPrefix(calls[1].Text, "[craft:delivery:first]\n") {
		t.Fatal(calls)
	}
	d.Script(Step{Op: Stop, Outcome: host.Unknown}, Step{Op: Status, State: host.Gone})
	if err := d.Stop(t.Context(), s, time.Second); !errors.Is(err, host.Unknown) {
		t.Fatal(err)
	}
	if d.Status(t.Context(), s) != host.Gone || d.Append(t.Context(), s, "正文", "third").Outcome != host.Unknown {
		t.Fatal("会话丢失后仍可投递")
	}
}

func TestUnknownCreation(t *testing.T) {
	d := New(host.Capabilities{CreateSeparatesPrompt: true}, nil)
	d.Script(Step{Op: Create, Outcome: host.Unknown}, Step{Op: Create, Outcome: host.Unknown, ID: "known"})
	calls := 0
	spec := host.CreateSpec{Directory: t.TempDir(), OnCreated: func(s host.Session) error { calls++; return nil }}
	for _, id := range []string{"", "known"} {
		s, err := d.Create(t.Context(), spec)
		if !errors.Is(err, host.Unknown) || s.ID != id {
			t.Fatal(s, err)
		}
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestDelayedAppend(t *testing.T) {
	started := make(chan time.Duration, 1)
	tick := make(chan time.Time)
	d := New(host.Capabilities{}, func(delay time.Duration) <-chan time.Time { started <- delay; return tick })
	s, err := d.Create(t.Context(), host.CreateSpec{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d.Script(Step{Op: Append, Delay: time.Hour, Outcome: host.Unknown})
	result := make(chan host.Result, 1)
	go func() { result <- d.Append(t.Context(), s, "x", "one") }()
	if <-started != time.Hour || len(d.Calls()) != 2 {
		t.Fatal("未登记延迟操作")
	}
	tick <- time.Unix(0, 0)
	if (<-result).Outcome != host.Unknown || d.Status(t.Context(), s) != host.Idle {
		t.Fatal("将未知结果视为送达")
	}
	ctx, cancel := context.WithCancel(t.Context())
	d.Script(Step{Op: Append, Delay: time.Hour})
	go func() { result <- d.Append(ctx, s, "x", "two") }()
	<-started
	cancel()
	if err := (<-result).Err; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func BenchmarkRoundTrip(b *testing.B) {
	for _, op := range []Op{Create, Append, Status} {
		b.Run(string(op), func(b *testing.B) {
			d := New(host.Capabilities{BusyAppend: true}, nil)
			ctx := b.Context()
			spec := host.CreateSpec{Directory: b.TempDir()}
			s, err := d.Create(ctx, spec)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				// 长基准只量一次调用；定期丢弃旧的断言记录，避免测成堆扩容。
				if len(d.calls) >= 4096 {
					b.StopTimer()
					d.calls = d.calls[:0]
					if op == Create {
						clear(d.sessions)
						clear(d.received)
					}
					b.StartTimer()
				}
				switch op {
				case Create:
					if _, err := d.Create(ctx, spec); err != nil {
						b.Fatal(err)
					}
				case Append:
					if r := d.Append(ctx, s, "正文", "sample"); r.Outcome != host.OK {
						b.Fatal(r)
					}
				case Status:
					if d.Status(ctx, s) == host.Gone {
						b.Fatal("会话丢失")
					}
				}
			}
		})
	}
}

func TestStopDeadlineUsesInjectedClock(t *testing.T) {
	armed := make(chan time.Duration, 1)
	tick := make(chan time.Time)
	d := New(host.Capabilities{}, func(delay time.Duration) <-chan time.Time { armed <- delay; return tick })
	s, err := d.Create(t.Context(), host.CreateSpec{Directory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	d.Script(Step{Op: Stop, Delay: 2 * time.Hour})
	done := make(chan error, 1)
	go func() { done <- d.Stop(t.Context(), s, time.Hour) }()
	if delay := <-armed; delay != time.Hour {
		t.Fatal(delay)
	}
	tick <- time.Unix(0, 0)
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, host.Unknown) {
		t.Fatal(err)
	}
}
