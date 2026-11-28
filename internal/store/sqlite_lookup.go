package store

import (
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/chhuax/craft/internal/store/migrate"
)

// 所有命名查询都返回键和可选 JSON 投影。只取标识的反查不装载聚合，
// 最近 N 项的排除集合只展开一次，不占用数量随输入增长的绑定参数。
var lookups = map[string]string{
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
