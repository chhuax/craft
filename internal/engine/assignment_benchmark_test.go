package engine

import (
	"encoding/json"
	"fmt"
	"testing"
)

func BenchmarkAssignmentCommands(b *testing.B) {
	b.Setenv("CRAFT_HOME", b.TempDir())
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d/Works_1000", count), func(b *testing.B) {
			s := seedRepository(b, count).(*memoryStore)
			ctx := b.Context()
			e, err := New(Config{Store: s, Clock: func() string { return at2 }, Policy: func() Policy { return Policy{} }})
			if err != nil {
				b.Fatal(err)
			}
			id := "work-0001"
			ref := NewThreadRef("source", "item", "00001")
			a := "run"
			w := Work{ID: id, Origin: Origin{TriggerID: "origin"}, Attention: &Question{ID: "q"}, Assignments: []Assignment{{ID: a, State: AssignmentRunning}}, Follows: []Follow{{Own: true, Thread: ThreadKey(ref)}}}
			for i := range 510 {
				inID := fmt.Sprintf("input-%04d", i)
				in := WorkInput{ID: inID, Kind: "trigger", TriggerID: inID, At: at1}
				if i < 500 {
					in.DeliveredTo = a
					in.DeliveredAt = at1
					in.ReadAt = ReadTime{Present: true, Time: at1}
				} else {
					w.Undelivered = append(w.Undelivered, in.ID)
				}
				w.Inputs = append(w.Inputs, in)
			}
			err = s.Atomic(ctx, func(u Unit) error {
				if err := u.PutTrigger(Trigger{ID: "origin", Threads: []ThreadRef{ref}, At: at1, Human: true, Channel: &ChannelRef{Source: "source", Thread: json.RawMessage(`"room"`)}}); err != nil {
					return err
				}
				return u.PutWork(w)
			})
			if err != nil {
				b.Fatal(err)
			}
			initial := s.committed.Load()
			ids := w.Undelivered
			if marker, err := e.PrepareDelivery(ctx, id, a, ids); err != nil || marker == "" {
				b.Fatalf("准备初始批次 %q: %v", marker, err)
			}
			prepared := s.committed.Load()
			for _, tc := range []struct {
				name string
				view *memoryView
				run  func() error
			}{
				{"PrepareDelivery", initial, func() error { _, err := e.PrepareDelivery(ctx, id, a, ids); return err }},
				{"InputsDelivered", prepared, func() error { return e.InputsDelivered(ctx, id, a, ids, true, at2) }},
				{"Report", initial, func() error {
					_, err := e.Report(ctx, id, a, WorkerReport{State: Progress, Summary: "核对进度"})
					return err
				}},
				{"Reply", initial, func() error {
					_, err := e.Reply(ctx, ReplyRequest{Kind: "acknowledgement", WorkID: id, TriggerID: "origin", Text: "已收到"})
					return err
				}},
				{"Answer", initial, func() error { _, err := e.Answer(ctx, id, "继续"); return err }},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						// 复位已提交快照，确保每次都实际写入；回执场景从已准备的同一批开始。
						s.committed.Store(tc.view)
						if err := tc.run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
