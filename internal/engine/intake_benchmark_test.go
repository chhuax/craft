package engine

import (
	"encoding/json"
	"fmt"
	"testing"
)

type benchScope struct{ listened bool }

func (s benchScope) Scope(string, string, map[string]json.RawMessage) ScopeResult {
	return ScopeResult{Listened: s.listened}
}

func (benchScope) Closable(string, string) bool { return false }

func BenchmarkReceive(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d", n), func(b *testing.B) {
			s := seedRepository(b, n).(*memoryStore)
			tr := Trigger{ID: "e1", Source: "source", Event: "change", At: at2,
				Threads: []ThreadRef{NewThreadRef("source", "item", "00001"), NewThreadRef("source", "item", "00002")}}
			err := s.Atomic(b.Context(), func(u Unit) error {
				th, err := u.GetThread(ThreadKey(tr.Threads[0]))
				if err != nil {
					return err
				}
				th.Subscriptions = []string{"work-0001", "work-0002", "work-0003"}
				return u.PutThread(*th)
			})
			if err != nil {
				b.Fatal(err)
			}
			seed := s.committed.Load()
			p := buildPolicy(b, Settings{}, EventDef{Source: tr.Source, Event: tr.Event, Default: ModeAuto})
			for _, path := range []ReceiveResult{Stored, Duplicate, Filtered} {
				b.Run(string(path), func(b *testing.B) {
					s.committed.Store(seed)
					e, err := New(Config{Store: s, Scope: benchScope{path != Filtered}, Clock: func() string { return at2 }, Policy: func() Policy { return p }})
					if err != nil {
						b.Fatal(err)
					}
					ctx := b.Context()
					if path == Duplicate {
						if _, err := e.Receive(ctx, tr); err != nil {
							b.Fatal(err)
						}
					}
					base := s.committed.Load()
					b.ReportAllocs()
					for b.Loop() {
						// 每次都真实提交，再用只读快照复位。固定输入历史长度，单独比较库的基数。
						s.committed.Store(base)
						got, err := e.Receive(ctx, tr)
						if err != nil || got != path {
							b.Fatalf("处置 %q，错误 %v", got, err)
						}
					}
				})
			}
		})
	}
}
