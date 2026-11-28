package engine

import (
	"fmt"
	"testing"
)

func BenchmarkWorkLifecycle(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d/Works_1000", count), func(b *testing.B) {
			s := seedRepository(b, count).(*memoryStore)
			ctx := b.Context()
			ref := NewThreadRef("source", "item", "00001")
			target := NewThreadRef("source", "item", "00002")
			key := ThreadKey(ref)
			ids := []string{"work-0001", "work-0002", "work-0003"}
			p := buildPolicy(b, Settings{})
			e, err := New(Config{Store: s, Scope: evaluationScope{benchScope{true}}, Clock: func() string { return at2 }, Policy: func() Policy { return p }})
			if err != nil {
				b.Fatal(err)
			}
			err = s.Atomic(ctx, func(u Unit) error {
				// 共享订阅，一个 own 持有人、两个旁观者；各次迭代都实际修改同一组聚合。
				if err := u.PutThread(Thread{Ref: ref, Key: key, Subscriptions: ids}); err != nil {
					return err
				}
				if err := u.PutThread(Thread{Ref: target, Key: ThreadKey(target), Subscriptions: ids[1:]}); err != nil {
					return err
				}
				for i, id := range ids {
					w := Work{ID: id, CreatedAt: at1, Follows: []Follow{{Thread: key}}}
					if i == 0 {
						w.Follows[0].Own = true
						w.Follows[0].Via = []string{"change"}
						w.Assignments = []Assignment{{ID: "run", State: AssignmentRunning, CreatedAt: at1, Report: &Report{State: Ready, At: at1}}}
						w.Execution = &Execution{Session: &Session{Host: "local", ID: "session"}}
						w.Inputs = []WorkInput{{Kind: "trigger", ID: "input", TriggerID: "change", Human: true, At: at1}}
					}
					if err := u.PutWork(w); err != nil {
						return err
					}
				}
				if err := u.PutTrigger(Trigger{ID: "change", At: at1, Threads: []ThreadRef{ref}, Human: true}); err != nil {
					return err
				}
				return u.PutDisposition("change", TriggerDisposition{At: at1, Fanout: ids[:1]})
			})
			if err != nil {
				b.Fatal(err)
			}
			initial := s.committed.Load()
			for _, tc := range []struct {
				name string
				run  func() error
			}{
				{"ThreadClosed", func() error { return e.ThreadClosed(ctx, ref, "done", at2, "") }},
				{"BindThread", func() error { _, err := e.BindThread(ctx, ids[0], target, true); return err }},
				{"StopWork", func() error { _, err := e.StopWork(ctx, ids[0], "结束"); return err }},
				{"Handback", func() error {
					ok, err := e.Handback(ctx, ids[0], "change", "另行研判")
					if err == nil && !ok {
						return fmt.Errorf("事件未重排")
					}
					return err
				}},
			} {
				b.Run(tc.name, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						// 复位快照只交换指针，无需重建库，排除重复命令提前返回的快路径。
						s.committed.Store(initial)
						if err := tc.run(); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
