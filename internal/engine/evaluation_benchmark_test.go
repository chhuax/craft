package engine

import (
	"fmt"
	"testing"
)

type evaluationScope struct{ benchScope }

func (evaluationScope) Closable(_ string, kind string) bool { return kind == "item" }

func BenchmarkAssessment(b *testing.B) {
	for _, size := range []struct{ threads, works int }{{100, 1000}, {10000, 1000}, {10000, 10000}} {
		b.Run(fmt.Sprintf("Threads_%d/Works_%d", size.threads, size.works), func(b *testing.B) {
			s := seedRepository(b, size.threads).(*memoryStore)
			p := buildPolicy(b, Settings{}, EventDef{Source: "source", Event: "assign", Strength: Explicit, Default: ModeAuto})
			e, err := New(Config{Store: s, Policy: func() Policy { return p }, Clock: func() string { return at2 }, Scope: evaluationScope{benchScope{true}}})
			if err != nil {
				b.Fatal(err)
			}
			ctx := b.Context()
			err = s.Atomic(ctx, func(u Unit) error {
				for i := 1000; i < size.works; i++ {
					if err := u.PutWork(Work{ID: fmt.Sprintf("extra-%05d", i), CreatedAt: at2}); err != nil {
						return err
					}
				}
				for i := range 10 {
					ref := NewThreadRef("source", "item", fmt.Sprintf("%05d", i*(size.threads/10)))
					th, err := u.GetThread(ThreadKey(ref))
					if err != nil {
						return err
					}
					th.Pending = nil
					// 保留真实的订阅邻居以计入候选和互斥校验的读取成本。
					th.Subscriptions = []string{"work-0001", "work-0002"}
					for j := range 3 {
						id := fmt.Sprintf("event-%02d-%d", i, j)
						th.Pending = append(th.Pending, id)
						tr := Trigger{ID: id, Source: "source", Event: "assign", Threads: []ThreadRef{ref}, At: at2, Text: "核对变化"}
						if err := u.PutTrigger(tr); err != nil {
							return err
						}
						if err := u.PutDisposition(id, TriggerDisposition{At: at2}); err != nil {
							return err
						}
					}
					if err := u.PutThread(*th); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				b.Fatal(err)
			}
			pending := s.committed.Load()
			if n, err := e.EvaluateDue(ctx); err != nil || n != 10 {
				b.Fatalf("准备批次 %d %v", n, err)
			}
			evaluating := s.committed.Load()
			th, err := evaluating.GetThread("source:item:00000")
			if err != nil {
				b.Fatal(err)
			}
			ev := th.Evaluating
			b.Run("EvaluateDue", func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					// 每次从同一已提交快照开始，复位不会复制库，也不会累积派工或历史。
					s.committed.Store(pending)
					if n, err := e.EvaluateDue(ctx); err != nil || n != 10 {
						b.Fatalf("处理 %d %v", n, err)
					}
				}
			})
			b.Run("Candidate", func(b *testing.B) {
				s.committed.Store(evaluating)
				b.ReportAllocs()
				for b.Loop() {
					if c, err := e.Candidate(ctx, ev.ID); err != nil || len(c.Related) != 8 {
						b.Fatalf("候选 %d %v", len(c.Related), err)
					}
				}
			})
			for _, kind := range []DecisionKind{Accept, Ignore} {
				b.Run("Decide_"+string(kind), func(b *testing.B) {
					out := Outcome{Kind: kind, EvalID: ev.ID, TriggerID: ev.Primary, Reason: Reason{Code: ReasonSaid}}
					b.ReportAllocs()
					for b.Loop() {
						s.committed.Store(evaluating)
						if d, err := e.Decide(ctx, out); err != nil || d.Kind != kind {
							b.Fatalf("决定 %s %v", d.Kind, err)
						}
					}
				})
			}
		})
	}
}
