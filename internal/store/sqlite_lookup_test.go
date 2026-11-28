package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupPlans(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "plans.db"))
	// 直接解释生产查询，允许遍历调用方传入的 JSON 键数组。
	// 聚合和反查表必须按索引搜索，排序也应由索引满足。
	expected := map[string]string{
		"pending": "thread_pending", "evaluating": "thread_eval", "eval": "thread_eval",
		"open": "watch_work_active", "recent": "work_recent", "origin": "work_origin",
		"subscriptions": "subscription_work", "threadKeys": "link_trigger", "triggerIDs": "link_thread",
		"decisions": "decision_thread", "accepted": "acceptance_trigger", "events": "event_rev",
	}
	for name, query := range lookups {
		t.Run(name, func(t *testing.T) { checkPlan(t, s, query, expected[name]) })
	}
	for _, collection := range []string{"watch_trigger", "watch_disposition", "watch_thread", "watch_work", "watch_decision"} {
		t.Run(collection, func(t *testing.T) { checkPlan(t, s, manyQuery(collection), "sqlite_autoindex_"+collection) })
	}
	checkPlan(t, s, effectHead, "effect_order")
	checkPlan(t, s, effectDue, "effect_due")
}

func checkPlan(t *testing.T, s *SQLite, query, index string) {
	t.Helper()
	rows, err := s.reader.Query("EXPLAIN QUERY PLAN "+query, make([]any, strings.Count(query, "?"))...)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, rows)
	var steps []string
	found := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
		if strings.HasPrefix(detail, "SEARCH ") && index != "" && strings.Contains(detail, "INDEX "+index) {
			found = true
		}
		if strings.HasPrefix(detail, "SCAN ") && !strings.Contains(detail, "VIRTUAL TABLE") || strings.Contains(detail, "TEMP B-TREE FOR ORDER BY") {
			t.Errorf("出现全表扫描或额外排序：%s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Errorf("未按 %s 查找：%v", index, steps)
	}
}
