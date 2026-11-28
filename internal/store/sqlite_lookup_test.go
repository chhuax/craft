package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLookupPlans(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "plans.db"))
	// 直接解释生产查询，允许展开请求的键数组或按主键命中的单份文档。
	// 聚合和反查表必须按索引搜索，排序也应由索引满足。
	const workKey = "INDEX sqlite_autoindex_watch_work_1 (key=?)"
	const inputKey = "PRIMARY KEY (work_key=? AND input_id=?)"
	expected := map[string][]string{
		"executorRunning": {"INDEX work_recent"}, "executorSignals": {"key>? AND key<?"}, "executorOpen": {"INDEX work_recent"},
		"totals":   {"PRIMARY KEY (name=?)"},
		"workPage": {"sqlite_autoindex_watch_work_1"}, "openPage": {"work_open_key"},
		"threadPage":     {"PRIMARY KEY (thread_key=? AND work_id>?)", "sqlite_autoindex_watch_work_1", "work_origin_key"},
		"openThreadPage": {"PRIMARY KEY (thread_key=? AND work_id>?)", "work_open_key", "work_origin_key"},
		"replyFailures":  {"key>? AND key<?"},
		"attentionWorks": {"work_attention"}, "warningKeys": {"decision_warning"},
		"decisionByKind": {"decision_kind"}, "decisionBySource": {"decision_source"}, "observedDecisions": {"decision_observed"},
		"sessionWorks": {"work_session"}, "decisionPage": {"sqlite_autoindex_watch_decision_1"},
		"requests": {"INDEX alarm_request_status"}, "subscribed": {"INDEX thread_subscribed", "INDEX sqlite_autoindex_watch_work_1"},
		"stateGet": {"PRIMARY KEY"}, "stateKeys": {"PRIMARY KEY"},
		"outboxList": {"INDEX sqlite_autoindex_outbox_1"}, "outboxReceipts": {"INDEX outbox_receipts"}, "outboxCounts": {"INDEX outbox_state"},
		"outboxState": {"INDEX outbox_state"}, "autoWindow": {"INDEX outbox_auto_at"},
		"pending": {"INDEX thread_pending"}, "evaluating": {"INDEX thread_eval"}, "eval": {"INDEX thread_eval"},
		"open": {"INDEX watch_work_active"}, "recent": {"INDEX work_recent"}, "origin": {"INDEX work_origin"},
		"subscriptions": {"INDEX subscription_work"}, "threadKeys": {"INDEX link_trigger"}, "triggerIDs": {"INDEX link_thread"},
		"decisions": {"INDEX decision_thread"}, "accepted": {"INDEX acceptance_trigger"}, "events": {"INDEX event_rev"},
		"deliveryWindow": {workKey}, "deliveryHead": {workKey}, "deliveryInputs": {workKey, inputKey},
		"deliveryState": {workKey}, "consumeHead": {workKey}, "deliveryInput": {workKey, inputKey},
	}
	for name, query := range lookups {
		t.Run(name, func(t *testing.T) { checkPlan(t, s, query, expected[name]...) })
	}
	for name, query := range updates {
		t.Run(name, func(t *testing.T) { checkPlan(t, s, query, expected[name]...) })
	}
	for _, collection := range []string{"watch_trigger", "watch_disposition", "watch_thread", "watch_work", "watch_decision"} {
		t.Run(collection, func(t *testing.T) { checkPlan(t, s, manyQuery(collection), "INDEX sqlite_autoindex_"+collection) })
	}
	checkPlan(t, s, getQuery("watch_thread"), "INDEX sqlite_autoindex_watch_thread_1 (key=?)")
	checkPlan(t, s, getQuery("outbox"), "INDEX sqlite_autoindex_outbox_1 (key=?)")
	checkPlan(t, s, getQuery("watch_reply_status"), "INDEX sqlite_autoindex_watch_reply_status_1 (key=?)")
	checkPlan(t, s, effectByKey, "INDEX sqlite_autoindex_watch_effect_1 (kind=? AND key=?)")
	checkPlan(t, s, inboxDelete, "INDEX sqlite_autoindex_watch_inbox_1 (id=?)")
	checkPlan(t, s, getQuery("watch_trigger"), "INDEX sqlite_autoindex_watch_trigger_1 (key=?)")
	checkPlan(t, s, inboxBatch, "INDEX inbox_order")
	checkPlan(t, s, inboxHead, "INDEX inbox_order")
	checkPlan(t, s, effectHead, "INDEX effect_order")
	checkPlan(t, s, effectDue, "INDEX effect_due")
}

func checkPlan(t *testing.T, s *SQLite, query string, indexes ...string) {
	t.Helper()
	if len(indexes) == 0 {
		t.Fatal("缺少预期索引")
	}
	rows, err := s.reader.Query("EXPLAIN QUERY PLAN "+query, make([]any, strings.Count(query, "?"))...)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, rows)
	var steps []string
	found := make(map[string]bool)
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		steps = append(steps, detail)
		// 从 seq 索引头部顺读，LIMIT 命中后就停止，无须构造虚假的范围谓词。
		bounded := (query == inboxBatch || query == inboxHead) && strings.Contains(detail, "INDEX inbox_order")
		for _, index := range indexes {
			if (bounded || strings.HasPrefix(detail, "SEARCH ")) && strings.Contains(detail, index) {
				found[index] = true
			}
		}
		if !bounded && strings.HasPrefix(detail, "SCAN ") && !strings.Contains(detail, "VIRTUAL TABLE") || strings.Contains(detail, "TEMP B-TREE FOR ORDER BY") {
			t.Errorf("出现全表扫描或额外排序：%s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s: %s", query, strings.Join(steps, " / "))
	for _, index := range indexes {
		if !found[index] {
			t.Errorf("未按 %s 查找：%v", index, steps)
		}
	}
}
