package engine

import (
	"encoding/json"
	"fmt"
	"testing"
)

func seedRepository(b *testing.B, threads int) Store {
	b.Helper()
	s := NewMemory()
	err := s.Atomic(b.Context(), func(u Unit) error {
		for i := range threads {
			id := fmt.Sprintf("%05d", i)
			ref := NewThreadRef("source", "item", id)
			th := Thread{Key: ThreadKey(ref), Ref: ref, CreatedAt: at1}
			// 两种规模中固定十条待研判，其余历史记录只增大基数。
			if i%(threads/10) == 0 {
				th.Pending = []string{"e"}
			}
			if err := u.PutThread(th); err != nil {
				return err
			}
		}
		for i := range 1000 {
			if err := u.PutWork(Work{ID: fmt.Sprintf("work-%04d", i), CreatedAt: at1}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkMemoryAtomic(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d", n), func(b *testing.B) {
			s := seedRepository(b, n)
			keys := []string{"source:item:00001", "source:item:00042", "source:item:00093"}
			w := Work{ID: "work-0042", CreatedAt: at1}
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				err := s.Atomic(ctx, func(u Unit) error {
					rows, err := u.GetThreads(keys...)
					if err != nil {
						return err
					}
					for _, k := range keys {
						th := rows[k]
						th.LastAt = at2
						if err := u.PutThread(*th); err != nil {
							return err
						}
					}
					w.UpdatedAt = at2
					return u.PutWork(w)
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemoryActive(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d", n), func(b *testing.B) {
			s := seedRepository(b, n)
			ctx := b.Context()
			b.ReportAllocs()
			for b.Loop() {
				err := s.Read(ctx, func(r Reader) error {
					rows, err := r.ActiveThreadsWithPending()
					if err == nil && len(rows) != 10 {
						b.Fatalf("命中 %d 个活跃 Thread", len(rows))
					}
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkMemoryEmit(b *testing.B) {
	s := NewMemory()
	e := Event{Type: "work.created", At: at1, Ref: map[string]string{"work": "w"}, Data: map[string]json.RawMessage{"count": json.RawMessage("1")}}
	var seen int64
	s.Subscribe(func(e Event) { seen = e.Rev })
	ctx := b.Context()
	b.ReportAllocs()
	for b.Loop() {
		if err := s.Atomic(ctx, func(u Unit) error { return u.Emit(e) }); err != nil {
			b.Fatal(err)
		}
	}
	if seen != int64(b.N) {
		b.Fatalf("最后广播 rev = %d", seen)
	}
}
