package repotest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

const at = "2026-01-01T00:00:00.000Z"

func ownership(t *testing.T, s engine.Store) {
	ctx := t.Context()
	th := engine.Thread{Key: "k", Pending: []string{"e"}, Meta: engine.ThreadMeta{Facts: map[string]engine.Fact{"n": {Value: json.RawMessage("123")}}}}
	mutate := func(v *engine.Thread) {
		v.Pending[0] = "changed"
		v.Meta.Facts["n"].Value[0] = '9'
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutThread(th))
		mutate(&th)
		return nil
	}))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		for _, get := range []func() (*engine.Thread, error){
			func() (*engine.Thread, error) { return r.GetThread("k") },
			func() (*engine.Thread, error) { m, err := r.GetThreads("k"); return m["k"], err },
			func() (*engine.Thread, error) {
				v, err := r.ActiveThreadsWithPending()
				if len(v) != 1 {
					t.Fatalf("活跃数 %d", len(v))
				}
				return &v[0], err
			},
			func() (*engine.Thread, error) { return r.GetThread("k") },
		} {
			v, err := get()
			must(t, err)
			if v == nil || !slices.Equal(v.Pending, []string{"e"}) || string(v.Meta.Facts["n"].Value) != "123" {
				t.Fatalf("对象所有权泄漏 %+v", v)
			}
			mutate(v)
		}
		return nil
	}))
}

func links(t *testing.T, s engine.Store) {
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		for _, pair := range [][2]string{{"a", "k"}, {"b", "k"}, {"a", "k"}, {"c", "k:x"}, {"a", "k:x"}, {"d", "k\x00"}} {
			must(t, u.LinkTrigger(pair[0], pair[1]))
		}
		err := u.Atomic(t.Context(), func(child engine.Unit) error {
			must(t, child.LinkTrigger("a", "lost"))
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatal(err)
		}
		return nil
	}))
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		for k, want := range map[string][]string{"k": {"a", "b"}, "k:x": {"a", "c"}, "k\x00": {"d"}, "absent": nil} {
			got, err := r.TriggerIDs(k)
			must(t, err)
			slices.Sort(got)
			if !slices.Equal(got, want) {
				t.Errorf("对象 %q 关联 %v，期望 %v", k, got, want)
			}
		}
		got, err := r.ThreadKeys("a")
		must(t, err)
		slices.Sort(got)
		if !slices.Equal(got, []string{"k", "k:x"}) {
			t.Errorf("事件 a 关联的对象 %v", got)
		}
		return nil
	}))
}

func checkSets(t *testing.T, r engine.Reader, threads, works []string) {
	t.Helper()
	ts, err := r.ActiveThreadsWithPending()
	must(t, err)
	ws, err := r.OpenWorks()
	must(t, err)
	var tk, wk []string
	for _, v := range ts {
		tk = append(tk, v.Key)
	}
	for _, v := range ws {
		wk = append(wk, v.ID)
	}
	if !slices.Equal(tk, threads) || !slices.Equal(wk, works) {
		t.Errorf("活跃 Thread %v / Work %v，期望 %v / %v", tk, wk, threads, works)
	}
}

func active(t *testing.T, s engine.Store) {
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		for _, k := range []string{"z", "a", "m"} {
			must(t, u.PutThread(engine.Thread{Key: k, Pending: []string{"e"}}))
			must(t, u.PutWork(engine.Work{ID: k}))
		}
		checkSets(t, u, []string{"z", "a", "m"}, []string{"m", "a", "z"})
		for _, tc := range []struct {
			th   engine.Thread
			want []string
		}{
			{engine.Thread{Key: "m"}, []string{"z", "a"}},
			{engine.Thread{Key: "a", Evaluating: &engine.Evaluation{}, Pending: []string{"e"}}, []string{"z"}},
			{engine.Thread{Key: "z", Closed: &engine.ThreadClose{}, Pending: []string{"e"}}, nil},
			{engine.Thread{Key: "a", Pending: []string{"e"}}, []string{"a"}},
		} {
			must(t, u.PutThread(tc.th))
			checkSets(t, u, tc.want, []string{"m", "a", "z"})
		}
		must(t, u.PutWork(engine.Work{ID: "z", Ended: &engine.WorkEnd{Outcome: engine.Done}}))
		checkSets(t, u, []string{"a"}, []string{"m", "a"})
		must(t, u.PutWork(engine.Work{ID: "z"}))
		checkSets(t, u, []string{"a"}, []string{"m", "a", "z"})
		return nil
	}))
}

func event(label string) engine.Event {
	return engine.Event{Rev: 123, Type: label, At: at, Ref: map[string]string{"work": "w"}, Data: map[string]json.RawMessage{"n": json.RawMessage("123")}}
}

func stageEvents(t *testing.T, u engine.Unit) {
	t.Helper()
	ctx := t.Context()
	e := event("first")
	must(t, u.Emit(e))
	e.Ref["work"] = "lost"
	e.Data["n"][0] = '8'
	err := u.Atomic(ctx, func(c engine.Unit) error {
		must(t, c.Emit(event("lost")))
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	must(t, u.Atomic(ctx, func(c engine.Unit) error { return c.Emit(event("second")) }))
}

func checkEventPages(t *testing.T, r engine.Reader) {
	t.Helper()
	for _, tc := range []struct {
		since int64
		limit int
		want  []int64
	}{{0, 1, []int64{1}}, {1, 1, []int64{2}}, {2, 10, []int64{3}}, {3, 10, nil}, {0, 0, nil}} {
		rows, err := r.EventsSince(tc.since, tc.limit)
		must(t, err)
		var revs []int64
		for _, e := range rows {
			revs = append(revs, e.Rev)
		}
		if !slices.Equal(revs, tc.want) {
			t.Errorf("游标 %d 上限 %d 得到 %v", tc.since, tc.limit, revs)
		}
	}
}

func events(t *testing.T, s engine.Store) {
	ctx := t.Context()
	var seen []engine.Event
	s.Subscribe(func(ev engine.Event) {
		ev.Ref["work"] = "changed"
		ev.Data["n"][0] = '9'
		panic("订阅者故障")
	})
	s.Subscribe(func(ev engine.Event) {
		// 广播时可登记订阅，也可从另一个读事务补拉同一条已提交记录。
		s.Subscribe(func(engine.Event) {})
		must(t, s.Read(ctx, func(r engine.Reader) error {
			rows, err := r.EventsSince(ev.Rev-1, 1)
			must(t, err)
			if len(rows) != 1 || !reflect.DeepEqual(rows[0], ev) {
				t.Errorf("广播早于事件提交，rev = %d", ev.Rev)
			}
			return nil
		}))
		seen = append(seen, ev)
	})
	for _, abort := range []bool{true, false} {
		err := s.Atomic(ctx, func(u engine.Unit) error {
			stageEvents(t, u)
			got, err := u.EventsSince(0, 10)
			must(t, err)
			if len(got) != 2 || len(seen) != 0 {
				t.Fatalf("提交前落盘 %d 条，广播 %d 条", len(got), len(seen))
			}
			if abort {
				return errRollback
			}
			return nil
		})
		if abort {
			if !errors.Is(err, errRollback) || len(seen) != 0 {
				t.Fatalf("回滚仍广播 %v，错误 %v", seen, err)
			}
		} else {
			must(t, err)
		}
	}
	// 已有提交后再回滚，下一次提交须从持久的计数器继续。
	err := s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.Emit(event("lost")))
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.Emit(event("third")) }))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		all, err := r.EventsSince(0, 10)
		must(t, err)
		if !reflect.DeepEqual(seen, all) || len(all) != 3 {
			t.Fatalf("广播 %v，事件记录 %v", seen, all)
		}
		for i, label := range []string{"first", "second", "third"} {
			want := event(label)
			want.Rev = int64(i + 1)
			if !reflect.DeepEqual(all[i], want) {
				t.Errorf("事件 %d = %+v", i, all[i])
			}
		}
		checkEventPages(t, r)
		return nil
	}))
}

func assessmentIndexes(t *testing.T, s engine.Store) {
	ctx := t.Context()
	th := engine.Thread{Key: "one", Evaluating: &engine.Evaluation{ID: "first"}}
	d := engine.Decision{ID: "choice", ThreadKey: th.Key, TriggerIDs: []string{"a", "b"}, Kind: engine.Accept, WorkID: "w"}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutThread(th))
		must(t, u.PutDecision(d))
		for _, w := range []engine.Work{{ID: "middle", CreatedAt: "2"}, {ID: "latest", CreatedAt: "3"}, {ID: "oldest", CreatedAt: "1"}, {ID: "ended", CreatedAt: "4", Ended: &engine.WorkEnd{}}} {
			must(t, u.PutWork(w))
		}
		return nil
	}))
	check := func(r engine.Reader) {
		got, err := r.ThreadByEval("first")
		must(t, err)
		if got == nil || got.Key != th.Key {
			t.Fatalf("研判索引 %+v", got)
		}
		ds, err := r.AcceptedByTrigger("a", "b", "absent")
		must(t, err)
		if len(ds) != 2 || ds["a"].DecisionID != d.ID || ds["b"].WorkID != "w" {
			t.Fatalf("已接事件索引 %+v", ds)
		}
		history, err := r.DecisionsByThread(th.Key)
		must(t, err)
		if len(history) != 1 || history[0].ID != d.ID {
			t.Fatalf("对象历史 %+v", history)
		}
		for _, tc := range []struct {
			limit int
			skip  []string
			want  []string
		}{
			{0, nil, nil}, {-1, nil, nil}, {1, nil, []string{"latest"}}, {3, []string{"latest", "missing"}, []string{"middle", "oldest"}},
		} {
			rows, err := r.RecentOpenWorks(tc.limit, tc.skip...)
			must(t, err)
			var ids []string
			for _, w := range rows {
				ids = append(ids, w.ID)
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("最近在办 %v，期望 %v", ids, tc.want)
			}
		}
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		check(u)
		err := u.Atomic(ctx, func(c engine.Unit) error {
			th.Evaluating = &engine.Evaluation{ID: "second"}
			must(t, c.PutThread(th))
			th.Evaluating = nil
			must(t, c.PutThread(th))
			for _, id := range []string{"first", "second"} {
				v, err := c.ThreadByEval(id)
				must(t, err)
				if v != nil {
					t.Fatalf("残留研判 %s", id)
				}
			}
			d.Kind = engine.Ignore
			d.ThreadKey = "two"
			must(t, c.PutDecision(d))
			ds, err := c.AcceptedByTrigger("a", "b")
			must(t, err)
			history, err := c.DecisionsByThread(th.Key)
			must(t, err)
			if len(ds)+len(history) != 0 {
				t.Fatal("改写后遗留决策索引")
			}
			must(t, c.PutWork(engine.Work{ID: "latest", Ended: &engine.WorkEnd{}}))
			must(t, c.PutWork(engine.Work{ID: "ended", CreatedAt: "0"}))
			must(t, c.PutWork(engine.Work{ID: "oldest", CreatedAt: "9"}))
			must(t, c.PutWork(engine.Work{ID: "after", CreatedAt: "9"}))
			rows, err := c.RecentOpenWorks(5)
			must(t, err)
			if len(rows) != 4 || rows[0].ID != "after" || rows[1].ID != "oldest" || rows[3].ID != "ended" {
				t.Fatalf("改写后的顺序 %v", rows)
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			return err
		}
		check(u)
		return nil
	}))
}
