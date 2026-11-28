package app

import (
	"encoding/json"
	"slices"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

// 可选排序只看跨来源约定的两个事实值，同优先级始终保留排队顺序。
type launchFacts struct{ urgency, due engine.Fact }

func (h executorHandler) selectDue(r store.SQLiteReader, now time.Time, limit int) ([]store.Effect, time.Time, error) {
	if h.kind != "launch" {
		return unorderedEffects(r, h.kind, now, limit)
	}
	n := limit
	if h.x.launchOrder != nil {
		n = -1
	}
	rows, err := r.DueLaunches(now, n)
	if err != nil {
		return nil, time.Time{}, err
	}
	if h.x.launchOrder != nil {
		facts := make(map[string]launchFacts, len(rows))
		for _, row := range rows {
			var p assignmentRef
			if err := json.Unmarshal(row.Value, &p); err != nil {
				return nil, time.Time{}, err
			}
			f, err := orderFacts(engineReader{r}, p.WorkID)
			if err != nil {
				return nil, time.Time{}, err
			}
			facts[row.Key] = f
		}
		slices.SortStableFunc(rows, func(a, b store.Effect) int { return h.x.launchOrder(facts[a.Key], facts[b.Key]) })
		rows = rows[:min(len(rows), limit)]
	}
	if len(rows) == limit {
		return rows, time.Time{}, nil
	}
	next, err := r.NextEffectDue(h.kind, now)
	return rows, next, err
}
func orderFacts(r engineReader, id string) (launchFacts, error) {
	var f launchFacts
	w, err := r.GetWork(id)
	if err != nil || w == nil {
		return f, err
	}
	t, err := r.GetTrigger(w.Origin.TriggerID)
	if err != nil || t == nil || len(t.Threads) == 0 {
		return f, err
	}
	th, err := r.GetThread(engine.ThreadKey(t.Threads[0]))
	if err != nil || th == nil {
		return f, err
	}
	return launchFacts{th.Meta.Facts[engine.FactUrgency], th.Meta.Facts[engine.FactDue]}, nil
}
