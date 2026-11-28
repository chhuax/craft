package store

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/chhuax/craft/internal/store/migrate"
)

// 订阅随退订而消失，按对象分页还须合并起源索引，保留已经结束的工作。
func threadPage(open bool) string {
	active := ""
	if open {
		active = " AND w.ended IS NULL"
	}
	return `SELECT s.work_id,w.value FROM watch_subscription s CROSS JOIN watch_work w ON w.key=s.work_id
		WHERE s.thread_key=?1 AND s.work_id>?2 AND w.origin_thread IS NOT ?1` + active + `
		UNION ALL SELECT w.key,w.value FROM watch_work w WHERE origin_thread=?1 AND w.key>?2` + active + `
		ORDER BY 1 LIMIT ?3`
}

// 所有命名查询都返回键和可选 JSON 投影。只取标识的反查不装载聚合，
// 最近 N 项的排除集合只展开一次，不占用数量随输入增长的绑定参数。
var lookups = map[string]string{
	"executorRunning": `SELECT key,value FROM watch_work INDEXED BY work_recent WHERE ended IS NULL AND value->>'$.assignments[#-1].state'='running' ORDER BY created,ordinal`,
	"executorSignals": `SELECT key,value FROM watch_kv WHERE key>='turn_end.' AND key<'turn_end/'`,
	"executorOpen":    `SELECT key,value FROM watch_work WHERE ended IS NULL ORDER BY created,ordinal`,
	"totals":          "SELECT name,total FROM document_count WHERE name IN ('watch_thread','watch_work')",
	"workPage":        "SELECT key,value FROM watch_work WHERE key>? ORDER BY key LIMIT ?",
	"openPage":        "SELECT key,value FROM watch_work WHERE ended IS NULL AND key>? ORDER BY key LIMIT ?",
	"threadPage":      threadPage(false), "openThreadPage": threadPage(true),
	"attentionWorks":    `SELECT key,value FROM watch_work INDEXED BY work_attention WHERE ended IS NULL AND (value->'$.delivery' IS NOT NULL OR value->'$.attention' IS NOT NULL OR value->'$.assignments[#-1].report' IS NOT NULL)`,
	"warningKeys":       `SELECT DISTINCT thread_key,NULL FROM watch_decision INDEXED BY decision_warning WHERE thread_key>'' AND value->>'$.kind'='unavailable' AND coalesce(value->>'$.dismissed',0)=0`,
	"sessionWorks":      `SELECT key,value FROM watch_work WHERE value->>'$.execution.session.host'=? AND value->>'$.execution.session.id'=? ORDER BY ended LIMIT 2`,
	"decisionByKind":    `SELECT key,value FROM watch_decision WHERE value->>'$.kind'=? AND key>? ORDER BY key LIMIT ?`,
	"decisionBySource":  `SELECT key,value FROM watch_decision WHERE substr(thread_key,1,instr(thread_key,':')-1)=? AND key>? AND (?='' OR value->>'$.kind'=?) ORDER BY key LIMIT ?`,
	"observedDecisions": `SELECT key,value FROM watch_decision INDEXED BY decision_observed WHERE value->>'$.audit.would_accept'=1 AND key>? AND (?='' OR value->>'$.kind'=?) AND (?='' OR substr(thread_key,1,instr(thread_key,':')-1)=?) ORDER BY key LIMIT ?`,
	"decisionPage":      "SELECT key,value FROM watch_decision WHERE key>? ORDER BY key LIMIT ?",
	"requests":          "SELECT key,value FROM watch_alarm_request WHERE status=? ORDER BY ordinal LIMIT ?",
	"stateGet":          "SELECT key,value FROM ext_state WHERE namespace=? AND key=?",
	"stateKeys":         "SELECT key,NULL FROM ext_state WHERE namespace=? ORDER BY key",
	"subscribed": `SELECT t.key,t.value FROM watch_thread t WHERE t.key>=?1 AND t.key<?2
		AND json_array_length(t.value,'$.subscriptions')>0
		AND (coalesce(nullif(t.value->>'$.lastAt',''),t.value->>'$.createdAt')>=?3 OR EXISTS (
			SELECT 1 FROM json_each(t.value,'$.subscriptions') s CROSS JOIN watch_work w
			WHERE w.key=s.value AND (w.ended IS NULL OR w.value->>'$.ended.at'>=?3))) ORDER BY t.key`,
	"outboxList": "SELECT key,value FROM outbox WHERE key>'' ORDER BY key",
	"outboxReceipts": `SELECT key,json_object('id',key,'action',value->>'$.action','sentSeq',sent_seq,'receipt',value->'$.receipt') FROM outbox
		WHERE state='done' AND extension=? AND sent_seq>? ORDER BY sent_seq LIMIT ?`,
	"outboxCounts": `SELECT state,count(*) FROM outbox WHERE state IN ('pending','executing','unknown') GROUP BY state`,
	"outboxState":  "SELECT key,value FROM outbox WHERE state=? ORDER BY key",
	// 只数到上限，窗口内历史记录再多也不影响准入成本。
	"autoWindow": "SELECT key,NULL FROM outbox WHERE auto_at>? AND auto_at<=? LIMIT 1 OFFSET ?",
	"pending":    "SELECT key,value FROM watch_thread WHERE closed IS NULL AND ready=1 ORDER BY ordinal",
	"evaluating": "SELECT key,value FROM watch_thread WHERE eval_id>'' ORDER BY eval_id",
	"eval":       "SELECT key,value FROM watch_thread WHERE eval_id>'' AND eval_id=? LIMIT 1",
	"open":       "SELECT key,value FROM watch_work WHERE ended IS NULL ORDER BY ordinal DESC",
	"recent": `SELECT key,value FROM watch_work WHERE ended IS NULL AND key NOT IN (SELECT value FROM json_each(?))
		ORDER BY created DESC,ordinal DESC LIMIT ?`,
	"origin":        "SELECT key,NULL FROM watch_work WHERE ended IS NULL AND origin_thread=? ORDER BY ordinal DESC LIMIT 1",
	"subscriptions": "SELECT thread_key,NULL FROM watch_subscription WHERE work_id=? ORDER BY thread_key",
	"threadKeys":    "SELECT thread_key,NULL FROM watch_trigger_thread WHERE trigger_id=? ORDER BY thread_key",
	"triggerIDs":    "SELECT trigger_id,NULL FROM watch_trigger_thread WHERE thread_key=? ORDER BY trigger_id",
	"decisions":     "SELECT key,value FROM watch_decision WHERE thread_key=? ORDER BY key",
	"accepted": `SELECT ids.value,(SELECT json_array(decision_id,work_id)
		FROM watch_acceptance WHERE trigger_id=ids.value ORDER BY decision_id DESC LIMIT 1) FROM json_each(?) AS ids`,
	"events": `SELECT key,value FROM watch_event WHERE json_extract(value,'$.rev')>? ORDER BY json_extract(value,'$.rev') LIMIT ?`,
	"deliveryWindow": `SELECT key,json_object(
		'Ended',iif(json_type(w.value,'$.ended')='object',json('true'),json('false')),
		'Assignment',w.value->'$.assignments[#-1]', 'Failure',w.value->'$.delivery',
		'Batch',w.value->'$.deliveryBatch', 'UpdatedAt',w.value->>'$.updatedAt')
		FROM watch_work w WHERE key=?`,
	"deliveryHead": `SELECT q.value,NULL FROM watch_work w,json_each(w.value,'$.undelivered') q WHERE w.key=? LIMIT ?`,
	"deliveryInputs": `SELECT p.input_id,w.value->('$.inputs['||p.position||']')
		FROM watch_work w JOIN watch_work_input p ON p.work_key=w.key
		WHERE w.key=? AND p.input_id IN (SELECT value FROM json_each(?))`,
}

// 这些投影只改原位置的值，保留文档中未选中的字段和数组元素的身份。
var updates = map[string]string{
	"deliveryInput": `UPDATE watch_work AS w SET value=json_set(w.value,'$.inputs['||p.position||']',
		jsonb_patch(CASE WHEN ?1->'$.readAt' IS NULL
			THEN jsonb_remove(w.value->('$.inputs['||p.position||']'),'$.readAt')
			ELSE jsonb_set(w.value->('$.inputs['||p.position||']'),'$.readAt',?1->'$.readAt') END,
			json_object('deliveredAt',?1->>'$.deliveredAt','deliveredTo',?1->>'$.deliveredTo')))
		FROM watch_work_input p WHERE w.key=?2 AND p.work_key=w.key AND p.input_id=json_extract(?1,'$.id')`,
	"consumeHead": `UPDATE watch_work SET value=json_set(value,'$.undelivered',
		json((SELECT json_group_array(value) FROM json_each(watch_work.value,'$.undelivered') WHERE key>=?)))
		WHERE key=?`,
	"deliveryState": `UPDATE watch_work SET value=json_patch(jsonb_set(value,'$.assignments[#-1]',
		jsonb_patch(value->'$.assignments[#-1]',json_object('deliveredAt',nullif(?1->>'$.deliveredAt','')))),
		json_object('updatedAt',?1->>'$.updatedAt','delivery',?1->'$.delivery','deliveryBatch',?1->'$.deliveryBatch'))
		WHERE key=?2`,
}

// Update 只允许按已登记的键和路径更新，以免部分写入破坏文档的二级索引。
func (t *SQLiteTx) Update(name string, args ...any) error {
	query, ok := updates[name]
	if !ok {
		return fmt.Errorf("未注册的索引更新 %q", name)
	}
	if _, err := t.tx.ExecContext(t.ctx, query, args...); err != nil {
		return fmt.Errorf("更新 %s: %w", name, err)
	}
	return nil
}

// Lookup 按注册的查询名返回有序文档或键投影。参数只作为值绑定。
func (r SQLiteReader) Lookup(name string, args ...any) ([]Doc, error) {
	query, ok := lookups[name]
	if !ok {
		return nil, fmt.Errorf("未注册的索引查询 %q", name)
	}
	return r.docs(name, query, args...)
}

// GetMany 只读请求的键，重复键只返回一次，缺失项省略。
func (r SQLiteReader) GetMany(collection string, keys []string) ([]Doc, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return nil, fmt.Errorf("编码 %s 查询键: %w", table, err)
	}
	return r.docs(table, manyQuery(table), string(raw))
}

func manyQuery(table string) string {
	return "SELECT key,value FROM " + table + " WHERE key IN (SELECT value FROM json_each(?))"
}

func (r SQLiteReader) docs(name, query string, args ...any) (_ []Doc, err error) {
	rows, err := r.tx.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 %s: %w", name, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var result []Doc
	for rows.Next() {
		var doc Doc
		var raw []byte
		if err := rows.Scan(&doc.Key, &raw); err != nil {
			return nil, fmt.Errorf("读取 %s 投影: %w", name, err)
		}
		doc.Value = raw
		result = append(result, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 %s: %w", name, err)
	}
	return result, nil
}
