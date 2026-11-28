package migrate

import (
	"database/sql"
	"fmt"
	"strings"
)

// 日期索引直接定位过期候选。数组引用在写文档时投影，清理小批候选无需重读大量仍在保留期的 JSON。
func createPruneIndexes(tx *sql.Tx) error {
	for _, ddl := range []string{
		"CREATE INDEX prune_work_end ON watch_work(value->>'$.ended.at')",
		"CREATE INDEX prune_decision_at ON watch_decision(value->>'$.at')",
		"CREATE INDEX prune_trigger_at ON watch_trigger(value->>'$.at')",
		"CREATE INDEX prune_thread_close ON watch_thread(value->>'$.closed.at')",
		"CREATE INDEX prune_event_at ON watch_event(value->>'$.at')",
		`CREATE TABLE watch_prune_ref (
			source TEXT NOT NULL, owner TEXT NOT NULL, target TEXT NOT NULL, id TEXT NOT NULL,
			PRIMARY KEY(source,owner,target,id)) WITHOUT ROWID`,
		"CREATE INDEX prune_referenced ON watch_prune_ref(target,id,source,owner)",
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("建立保留期索引: %w", err)
		}
	}
	for _, source := range []string{"work", "decision", "thread", "trigger", "disposition", "effect"} {
		if err := indexPruneRefs(tx, source); err != nil {
			return fmt.Errorf("建立 %s 引用索引: %w", source, err)
		}
	}
	return nil
}

// 同一份投影用于旧库回填和后续增改。以原文主键撤销旧投影，可处理引用被覆盖或移除。
func indexPruneRefs(tx *sql.Tx, source string) error {
	table := "watch_" + source
	project := pruneSelect(source, "NEW", " FROM (SELECT 1)")
	clearRefs := "DELETE FROM watch_prune_ref WHERE source='" + source + "' AND owner="
	ddl := []string{
		pruneSelect(source, "d", " FROM "+table+" AS d"),
		"CREATE TRIGGER prune_" + source + "_delete AFTER DELETE ON " + table + " BEGIN " + clearRefs + pruneOwner(source, "OLD") + "; END",
	}
	if source == "work" {
		// 与 work_input_positions 一样，整份文档写入（含 UPSERT）才会改变这些引用。
		// 投递回执的原位 UPDATE 不改引用，不应为每个已读输入重建整份历史索引。
		ddl = append(ddl, "CREATE TRIGGER prune_work_put BEFORE INSERT ON watch_work BEGIN "+clearRefs+"NEW.key; "+project+" END")
	} else {
		ddl = append(ddl, "CREATE TRIGGER prune_"+source+"_insert AFTER INSERT ON "+table+" BEGIN "+project+" END",
			"CREATE TRIGGER prune_"+source+"_update AFTER UPDATE OF value ON "+table+" BEGIN "+clearRefs+pruneOwner(source, "OLD")+"; "+project+" END")
	}
	for _, q := range ddl {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

func pruneOwner(source, row string) string {
	if source == "effect" {
		return "json_array(" + row + ".kind," + row + ".key)"
	}
	return row + ".key"
}

func pruneSelect(source, row, from string) string {
	var parts []string
	add := func(kind, expr, join string) {
		parts = append(parts, "SELECT '"+source+"',"+pruneOwner(source, row)+",'"+kind+"',"+expr+" AS ref "+from+join+
			" WHERE typeof("+expr+")='text' AND "+expr+"<>''")
	}
	value := row + ".value"
	field := func(kind, path string) { add(kind, value+"->>'$."+path+"'", "") }
	each := func(path string) string { return ", json_each(" + value + ",'$." + path + "') AS item" }
	values := func(kind, path string) { add(kind, "item.value", each(path)) }
	objects := func(kind, path, expr string) {
		// 通用文档仓储也能保存旧形状；非对象元素没有可解释的子字段。
		add(kind, "CASE WHEN item.type='object' THEN "+expr+" END", each(path))
	}
	switch source {
	case "work":
		field("decision", "decisionId")
		field("trigger", "origin.triggerId")
		field("trigger", "attention.triggerId")
		values("trigger", "origin.triggerIds")
		values("trigger", "answers")
		objects("trigger", "inputs", "item.value->>'$.triggerId'")
		objects("decision", "inputs", "item.value->>'$.decisionId'")
		objects("thread", "follows", "item.value->>'$.thread'")
		add("trigger", "via.value", ", json_each("+value+",'$.follows') f, json_each(CASE WHEN f.type='object' THEN f.value END,'$.via') via")
	case "decision":
		field("work", "workId")
		field("thread", "threadKey")
		values("trigger", "triggerIds")
		objects("trigger", "reason", "item.value->>'$.triggerId'")
		objects("trigger", "reason", "item.value->>'$.values.trigger'")
		objects("work", "reason", "item.value->>'$.values.work'")
	case "thread":
		values("thread", "correlated")
		values("trigger", "pending")
		field("thread", "evaluating.threadKey")
		field("trigger", "evaluating.primary")
		values("trigger", "evaluating.triggerIds")
	case "trigger":
		objects("thread", "threads", "(item.value->>'$.source')||':'||(item.value->>'$.kind')||':'||(item.value->>'$.id')")
	case "disposition":
		field("decision", "decisionId")
		values("work", "fanout")
		values("work", "context")
		objects("work", "handbacks", "item.value->>'$.workId'")
		field("trigger", "filtered.triggerId")
	case "effect":
		// reply 的派生消息标识不是 Trigger 引用；渠道和正文已完整保存在载荷中。
		field("work", "workId")
		field("trigger", "triggerId")
		field("thread", "threadKey")
		field("decision", "decisionId")
		values("trigger", "triggerIds")
		add("work", "CASE WHEN "+row+".kind='delivery' THEN "+row+".key END", "")
	}
	// 重复的批内引用只占一个索引项；回填时也无需对整库引用排序去重。
	return "INSERT INTO watch_prune_ref " + strings.Join(parts, " UNION ALL ") + " ON CONFLICT DO NOTHING;"
}
