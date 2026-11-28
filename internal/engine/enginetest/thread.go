package enginetest

import (
	"reflect"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func threadStatus(t *testing.T, s engine.Store) {
	ref := engine.NewThreadRef("feed", "item", "a")
	key := engine.ThreadKey(ref)
	ctx := t.Context()
	e := engine.New(engine.Config{Store: s, Clock: func() string { return now }})
	var seen []engine.Event
	s.Subscribe(func(ev engine.Event) { seen = append(seen, ev) })

	const early = "2026-01-01T00:00:10Z"
	const middle = "2026-01-01T00:00:20.000Z"
	const late = "2026-01-01T00:00:30.000Z"
	closeFact := &engine.ThreadClose{Outcome: "done", At: late, Detail: "验收完成"}
	for _, tc := range []struct {
		name, outcome, at string
		closed            *engine.ThreadClose
		reopened          string
		changed           bool
	}{
		{"首次即重开", "", "2026-01-01T08:00:20+08:00", nil, middle, true},
		{"早于重开的关闭", "closed", early, nil, middle, false},
		{"更晚的关闭", "done", "2026-01-01T08:00:30+08:00", closeFact, middle, true},
		{"早于关闭的关闭", "closed", middle, closeFact, middle, false},
		{"早于关闭的重开", "", middle, closeFact, middle, false},
		{"再次重开", "", now, nil, now, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(seen)
			op := "thread.reopened"
			if tc.outcome == "" {
				must(t, e.ThreadReopened(ctx, ref, tc.at))
			} else {
				op = "thread.closed"
				must(t, e.ThreadClosed(ctx, ref, tc.outcome, tc.at, "验收完成"))
			}
			if tc.changed {
				if len(seen) != before+1 || seen[before].Type != op || seen[before].Ref["thread"] != key || seen[before].At != now {
					t.Fatalf("状态变更广播 %v", seen[before:])
				}
			} else if len(seen) != before {
				t.Errorf("旧事实仍产生事件 %v", seen[before:])
			}
			must(t, s.Read(ctx, func(r engine.Reader) error {
				th, err := r.GetThread(key)
				must(t, err)
				if th == nil || th.CreatedAt != now || th.Ref != ref || !reflect.DeepEqual(th.Closed, tc.closed) || th.ReopenedAt != tc.reopened {
					t.Fatalf("对象状态 %+v，期望关闭 %+v、重开 %q", th, tc.closed, tc.reopened)
				}
				return nil
			}))
		})
	}
}
