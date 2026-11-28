package migrate

import (
	"database/sql"
	"fmt"
)

// 数组的每个元素都可能被反查。只在写入该文档时展开数组，使读者按键直达命中项。
// 索引和文档由同一条语句维护，PutMany、保存点和直接导入文档也具备相同语义。
func createIndexes(tx *sql.Tx) error {
	for _, ddl := range []string{
		"CREATE INDEX thread_pending ON watch_thread(closed,ordinal) WHERE closed IS NULL AND ready=1",
		"CREATE INDEX thread_eval ON watch_thread(eval_id) WHERE eval_id>''",
		"CREATE INDEX work_recent ON watch_work(ended,created DESC,ordinal DESC) WHERE ended IS NULL",
		"CREATE INDEX work_origin ON watch_work(origin_thread,ordinal DESC) WHERE ended IS NULL",
		"CREATE INDEX work_origin_id ON watch_work(origin_id) WHERE origin_id IS NOT NULL",
		`CREATE TABLE watch_work_input (
			work_key TEXT REFERENCES watch_work(key) ON DELETE CASCADE, input_id TEXT NOT NULL, position INTEGER NOT NULL,
			PRIMARY KEY(work_key,input_id)) WITHOUT ROWID`,
		// 整份替换与批量导入都走 INSERT ... ON CONFLICT，先为新布局建索引。
		// 回执只更新原位置的投递字段，走 UPDATE 时可以沿用这份位置索引。
		`CREATE TRIGGER work_input_positions BEFORE INSERT ON watch_work BEGIN
			DELETE FROM watch_work_input WHERE work_key=NEW.key;
			INSERT INTO watch_work_input(work_key,input_id,position)
				SELECT NEW.key,coalesce(item.value->>'$.id',''),item.key FROM json_each(NEW.value,'$.inputs') item
				WHERE item.type='object'
				ON CONFLICT(work_key,input_id) DO UPDATE SET position=max(position,excluded.position);
		END`,
		"CREATE INDEX event_rev ON watch_event(json_extract(value,'$.rev'))",
		"CREATE INDEX decision_thread ON watch_decision(thread_key,key)",
		"CREATE INDEX link_trigger ON watch_trigger_thread(trigger_id,thread_key)",
		"CREATE INDEX link_thread ON watch_trigger_thread(thread_key,trigger_id)",
		`CREATE TABLE watch_subscription (
			thread_key TEXT REFERENCES watch_thread(key) ON DELETE CASCADE, work_id TEXT NOT NULL,
			PRIMARY KEY(thread_key,work_id)) WITHOUT ROWID`,
		"CREATE INDEX subscription_work ON watch_subscription(work_id,thread_key)",
		`CREATE TABLE watch_acceptance (
			decision_id TEXT REFERENCES watch_decision(key) ON DELETE CASCADE, trigger_id TEXT NOT NULL, work_id TEXT NOT NULL,
			PRIMARY KEY(decision_id,trigger_id)) WITHOUT ROWID`,
		"CREATE INDEX acceptance_trigger ON watch_acceptance(trigger_id,decision_id,work_id)",
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("创建文档二级索引: %w", err)
		}
	}

	return createProjectionTriggers(tx)
}

func createProjectionTriggers(tx *sql.Tx) error {
	// 起源事件允许晚于 Work 导入。两端写入都刷新投影，退订不影响起源。
	for _, projection := range []struct{ table, body string }{
		{"watch_work", `WHEN NEW.origin_id IS NOT NULL OR NEW.origin_thread IS NOT NULL BEGIN
				UPDATE watch_work SET origin_thread=(SELECT primary_thread FROM watch_trigger WHERE key=NEW.origin_id)
				WHERE ordinal=NEW.ordinal; END`},
		{"watch_trigger", `BEGIN UPDATE watch_work SET origin_thread=NEW.primary_thread WHERE origin_id=NEW.key; END`},
		{"watch_thread", `BEGIN
				DELETE FROM watch_subscription WHERE thread_key=NEW.key;
				INSERT INTO watch_subscription SELECT NEW.key,value FROM (
					SELECT value FROM json_each(NEW.value,'$.subscriptions') WHERE type='text'
					UNION SELECT value FROM json_each(NEW.value,'$.context') WHERE type='text');
				END`},
		{"watch_decision", `BEGIN
				DELETE FROM watch_acceptance WHERE decision_id=NEW.key;
				INSERT INTO watch_acceptance SELECT DISTINCT NEW.key,value,json_extract(NEW.value,'$.workId')
				FROM json_each(NEW.value,'$.triggerIds') WHERE type='text'
				AND json_extract(NEW.value,'$.kind')='accept' AND json_extract(NEW.value,'$.workId')<>'';
				END`},
	} {
		for _, action := range []struct{ name, clause string }{{"insert", "INSERT"}, {"update", "UPDATE OF value"}} {
			if _, err := tx.Exec(fmt.Sprintf("CREATE TRIGGER %s_%s AFTER %s ON %s %s", projection.table, action.name, action.clause, projection.table, projection.body)); err != nil {
				return fmt.Errorf("创建 %s 的 %s 索引维护触发器: %w", projection.table, action.clause, err)
			}
		}
	}
	_, err := tx.Exec(`CREATE TRIGGER clear_origin AFTER DELETE ON watch_trigger BEGIN
		UPDATE watch_work SET origin_thread=NULL WHERE origin_id=OLD.key; END`)
	if err != nil {
		return fmt.Errorf("创建起源清理触发器: %w", err)
	}
	return nil
}
