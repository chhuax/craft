package app

import (
	"strconv"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

// BenchmarkPrune100k 在每个集合各十万条、有真实相互引用的记录中清理一批。
// 计时包括完整提交，每轮只在计时外重建上轮已删除的记录。
func BenchmarkPrune100k(b *testing.B) {
	for _, backend := range []string{"Memory", "SQLite"} {
		b.Run(backend, func(b *testing.B) {
			var s engine.Store
			if backend == "Memory" {
				s = engine.NewMemory()
			} else {
				s = sqliteRepository(b)
			}
			p := engine.Config{Store: s, Clock: func() string { return benchAt }, Policy: func() engine.Policy { return engine.Policy{} }}
			e, err := engine.New(p)
			benchCheck(b, err)
			const batch = 100
			seed := func(first, last int, at string) {
				b.Helper()
				benchCheck(b, s.Atomic(b.Context(), func(u engine.Unit) error {
					for i := first; i < last; i++ {
						id := strconv.Itoa(i)
						if err := u.PutTrigger(engine.Trigger{ID: id, At: at}); err != nil {
							return err
						}
						if err := u.PutDecision(engine.Decision{ID: id, At: at, WorkID: id, TriggerIDs: []string{id}}); err != nil {
							return err
						}
						if err := u.PutWork(engine.Work{ID: id, DecisionID: id, Ended: &engine.WorkEnd{At: at}, Origin: engine.Origin{TriggerID: id}}); err != nil {
							return err
						}
					}
					return nil
				}))
			}
			seed(batch, 100000, benchAt)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				seed(0, batch, "2026-01-01T00:00:00.000Z")
				b.StartTimer()
				got, err := e.Prune(b.Context(), benchAt, benchAt, benchAt)
				if err != nil || got.Works != batch || got.Decisions != batch || got.Triggers != batch {
					b.Fatalf("清理结果 %+v, %v", got, err)
				}
			}
		})
	}
}
