package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/host/fake"
)

func BenchmarkExecutorLaunch(b *testing.B) {
	for _, kind := range []string{"dir", "scratch"} {
		b.Run(kind, func(b *testing.B) {
			x, _, row := workerFixture(b, true)
			ctx := b.Context()
			pending, err := x.work(ctx, "work")
			checkErr(b, err)
			if kind == "scratch" {
				pending.Execution.Target = &engine.TargetSnapshot{ID: kind, Kind: kind}
			}
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				if kind == "scratch" {
					// 每次都计入新建 Work 目录的成本，父目录由各次派工共享。
					if err := os.Remove(filepath.Join(x.stateDir, "scratch", pending.ID)); !errors.Is(err, os.ErrNotExist) {
						checkErr(b, err)
					}
				}
				checkErr(b, x.r.repo.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(*pending) }))
				b.StartTimer()
				checkErr(b, x.run(ctx, row).Err)
			}
			w, err := x.work(ctx, pending.ID)
			checkErr(b, err)
			if latest(w).State != engine.AssignmentRunning {
				b.Fatal("基准派工没有启动", latest(w))
			}
		})
	}
}
func BenchmarkExecutorDeliveryBatch(b *testing.B) {
	x, _, row := workerFixture(b, true)
	ctx := b.Context()
	checkErr(b, x.run(ctx, row).Err)
	w, err := x.work(ctx, "work")
	checkErr(b, err)
	for i := range 10 {
		id := fmt.Sprint(i)
		w.Inputs = append(w.Inputs, engine.WorkInput{Kind: "trigger", ID: id, TriggerID: "event", At: "2020-01-01T00:00:00.000Z"})
		w.Undelivered = append(w.Undelivered, id)
	}
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		checkErr(b, x.r.repo.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(*w) }))
		b.StartTimer()
		checkErr(b, x.deliver(ctx, w.ID).Err)
	}
}
func BenchmarkExecutorMonitor100(b *testing.B) {
	x, d, _ := workerFixture(b, true)
	ctx := b.Context()
	w, err := x.work(ctx, "work")
	checkErr(b, err)
	checkErr(b, x.r.repo.Atomic(ctx, func(u engine.Unit) error {
		for i := range 100 {
			d.Script(fake.Step{Op: fake.Create, State: host.Busy})
			s, err := d.Create(ctx, host.CreateSpec{})
			if err != nil {
				return err
			}
			session := engine.Session(s)
			w.Execution.Session = &session
			w.ID = fmt.Sprint(i)
			w.Assignments[0].State = engine.AssignmentRunning
			if err := u.PutWork(*w); err != nil {
				return err
			}
		}
		return nil
	}))
	b.ReportAllocs()
	for b.Loop() {
		checkErr(b, x.sample(ctx, false))
	}
}
