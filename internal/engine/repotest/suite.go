// Package repotest 固定引擎仓储的可观察行为，供内存和持久化适配器共用。
package repotest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/engine"
)

// Run 对每项契约使用 factory 提供的独立空仓储；持久化适配器应在 t.TempDir 下建库。
func Run(t *testing.T, factory func(t *testing.T) engine.Store) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		check func(*testing.T, engine.Store)
	}{
		{"aggregates", aggregates}, {"ownership", ownership}, {"links", links},
		{"savepoints", savepoints}, {"callbacks", callbacks}, {"snapshot", snapshot},
		{"研判索引", assessmentIndexes}, {"active", active}, {"events", events}, {"concurrent", concurrent},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, factory(t)) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var errRollback = errors.New("撤销本层事务")

func savepoints(t *testing.T, s engine.Store) {
	ctx := t.Context()
	for _, abort := range []bool{true, false} {
		err := s.Atomic(ctx, func(u engine.Unit) error {
			must(t, u.PutThread(engine.Thread{Key: "k", Pending: []string{"outer"}}))
			err := u.Atomic(ctx, func(child engine.Unit) error {
				must(t, child.PutThread(engine.Thread{Key: "k"}))
				must(t, child.PutWork(engine.Work{ID: "lost"}))
				return errRollback
			})
			if !errors.Is(err, errRollback) {
				t.Fatalf("内层回滚返回 %v", err)
			}
			checkSets(t, u, []string{"k"}, nil)
			got, err := u.GetThread("k")
			must(t, err)
			if got == nil || !slices.Equal(got.Pending, []string{"outer"}) {
				t.Fatalf("内层改写未撤销：%+v", got)
			}
			must(t, u.Atomic(ctx, func(child engine.Unit) error {
				return child.PutWork(engine.Work{ID: strconv.FormatBool(abort)})
			}))
			if abort {
				return errRollback
			}
			return nil
		})
		if abort != errors.Is(err, errRollback) || (!abort && err != nil) {
			t.Fatalf("外层返回 %v", err)
		}
		must(t, s.Read(ctx, func(r engine.Reader) error {
			if abort {
				checkSets(t, r, nil, nil)
			} else {
				checkSets(t, r, []string{"k"}, []string{"false"})
			}
			ws, err := r.GetWorks("lost", "true")
			must(t, err)
			if len(ws) != 0 {
				t.Errorf("回滚后仍有 Work %v", ws)
			}
			return nil
		}))
	}
}

func callbacks(t *testing.T, s engine.Store) {
	// 有界等待使错误持锁的适配器能报告失败。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var seen []string
	for _, abort := range []bool{true, false} {
		err := s.Atomic(ctx, func(u engine.Unit) error {
			u.AfterCommit(func() { seen = append(seen, "first") })
			err := u.Atomic(ctx, func(child engine.Unit) error {
				child.AfterCommit(func() { seen = append(seen, "lost") })
				return errRollback
			})
			if !errors.Is(err, errRollback) {
				t.Fatal(err)
			}
			must(t, u.Atomic(ctx, func(child engine.Unit) error {
				child.AfterCommit(func() { seen = append(seen, "child"); panic("单个回调故障") })
				return nil
			}))
			u.AfterCommit(func() {
				// 回调可以重入写事务，并看到刚提交的记录。
				must(t, s.Atomic(ctx, func(next engine.Unit) error {
					w, err := next.GetWork("w")
					must(t, err)
					if w == nil {
						t.Error("回调看不到提交的 Work")
					}
					seen = append(seen, "last")
					return nil
				}))
			})
			must(t, u.PutWork(engine.Work{ID: "w"}))
			if abort {
				return errRollback
			}
			return nil
		})
		if abort {
			if !errors.Is(err, errRollback) || len(seen) != 0 {
				t.Fatalf("回滚仍调用回调 %v，错误 %v", seen, err)
			}
		} else {
			must(t, err)
		}
	}
	if !slices.Equal(seen, []string{"first", "child", "last"}) {
		t.Errorf("提交回调顺序 %v", seen)
	}
}

func snapshot(t *testing.T, s engine.Store) {
	// 有界等待使错误持锁的适配器能报告失败。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutThread(engine.Thread{Key: "k", Pending: []string{"old"}}) }))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		checkSets(t, r, []string{"k"}, nil)
		// 保持读快照，同时等另一个连接提交。
		done := make(chan error, 1)
		go func() {
			done <- s.Atomic(ctx, func(u engine.Unit) error {
				if err := u.PutThread(engine.Thread{Key: "k"}); err != nil {
					return err
				}
				return u.PutWork(engine.Work{ID: "w"})
			})
		}()
		must(t, <-done)
		checkSets(t, r, []string{"k"}, nil)
		got, err := r.GetThread("k")
		must(t, err)
		if got == nil || !slices.Equal(got.Pending, []string{"old"}) {
			t.Errorf("快照混入写入 %+v", got)
		}
		return nil
	}))
	must(t, s.Read(ctx, func(r engine.Reader) error { checkSets(t, r, nil, []string{"w"}); return nil }))
}

func concurrent(t *testing.T, s engine.Store) {
	ctx := t.Context()
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(engine.Work{ID: "w", Title: "0"}) }))
	const workers, times = 8, 16
	start := make(chan struct{})
	done := make(chan error, workers)
	for range workers {
		go func() {
			<-start
			for range times {
				if err := s.Atomic(ctx, increment); err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	close(start)
	for range workers {
		must(t, <-done)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		w, err := r.GetWork("w")
		must(t, err)
		if w == nil || w.Title != strconv.Itoa(workers*times) {
			t.Errorf("并发累加结果 %+v", w)
		}
		return nil
	}))
}

func increment(u engine.Unit) error {
	w, err := u.GetWork("w")
	if err != nil {
		return err
	}
	if w == nil {
		return errors.New("计数记录缺失")
	}
	n, err := strconv.Atoi(w.Title)
	if err != nil {
		return fmt.Errorf("读取计数: %w", err)
	}
	w.Title = strconv.Itoa(n + 1)
	return u.PutWork(*w)
}
