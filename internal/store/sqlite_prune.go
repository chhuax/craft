package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chhuax/craft/internal/engine"
)

// Prune 在调用者持有的写事务里按日期索引定位候选，再按反向引用索引核对保护根。
func (t *SQLiteTx) Prune(triggerBefore, workBefore, eventBefore string) (engine.PruneResult, error) {
	c := engine.PruneCandidates{}
	for _, kind := range []string{"work", "decision", "trigger", "thread"} {
		before := workBefore
		if kind == "trigger" {
			before = triggerBefore
		}
		if err := t.pruneCandidates(c, kind, before); err != nil {
			return engine.PruneResult{}, err
		}
	}
	expired := c.Expired()
	result := engine.PruneResult{WorkIDs: expired["work"]}
	for _, kind := range []string{"work", "decision", "trigger", "thread"} {
		if ids := expired[kind]; len(ids) > 0 {
			if err := t.deleteExpired(kind, ids, &result); err != nil {
				return engine.PruneResult{}, err
			}
		}
	}
	var err error
	result.Events, err = t.pruneEvents(eventBefore)
	return result, err
}

func pruneCandidateQuery(kind string) string {
	path, extra := "at", ""
	switch kind {
	case "work":
		path = "ended.at"
	case "trigger":
		// 迟到事件可能有更新的处置记录，同一组记录保留到两者都过期。
		extra = ` AND NOT EXISTS (SELECT 1 FROM watch_disposition d WHERE d.key=watch_trigger.key AND d.value->>'$.at'>=?1)`
	case "thread":
		// Thread 与 Work、Decision 同属长寿命记录；关闭后按较长的 workBefore 保留。
		path = "closed.at"
		extra = ` AND coalesce(json_array_length(value,'$.pending'),0)=0 AND value->>'$.evaluating' IS NULL`
	}
	date := "value->>'$." + path + "'"
	query := "WITH candidate AS (SELECT key FROM watch_" + kind + " WHERE " + date + ">'' AND " + date + "<?1" + extra + ") " +
		`SELECT c.key, r.source, r.owner FROM candidate c LEFT JOIN watch_prune_ref r ON r.target=?2 AND r.id=c.key`
	if kind == "thread" {
		// 订阅和显式事件关联已有双向索引，包括没有对应 Follow 的普通订阅。
		query += ` UNION ALL SELECT c.key,'work',s.work_id FROM candidate c CROSS JOIN watch_subscription s WHERE s.thread_key=c.key
			UNION ALL SELECT c.key,'trigger',l.trigger_id FROM candidate c CROSS JOIN watch_trigger_thread l WHERE l.thread_key=c.key`
	}
	return query
}

func (t *SQLiteTx) pruneCandidates(c engine.PruneCandidates, kind, before string) (err error) {
	rows, err := t.tx.QueryContext(t.ctx, pruneCandidateQuery(kind), before, kind)
	if err != nil {
		return fmt.Errorf("查询过期 %s: %w", kind, err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	for rows.Next() {
		var key string
		var source, owner sql.NullString
		if err := rows.Scan(&key, &source, &owner); err != nil {
			return fmt.Errorf("读取 %s 引用: %w", kind, err)
		}
		obj := engine.PruneObject{Kind: kind, ID: key}
		if source.Valid {
			// 处置与它所属的 Trigger 一起清理，共享保护链。
			if source.String == "disposition" {
				source.String = "trigger"
			}
			c[obj] = append(c[obj], engine.PruneObject{Kind: source.String, ID: owner.String})
		} else if _, ok := c[obj]; !ok {
			c[obj] = nil
		}
	}
	return rows.Err()
}

const expiredIDs = " IN (SELECT value FROM json_each(?1))"

// 只展开确有过期订阅的 Thread，每个数组按原顺序重建一次。
const removeSubscriptions = `UPDATE watch_thread SET value=json_patch(value,json_object(
	'subscriptions',json((SELECT json_group_array(value ORDER BY key) FROM json_each(watch_thread.value,'$.subscriptions') WHERE value NOT IN (SELECT value FROM json_each(?1)))),
	'context',json(nullif((SELECT json_group_array(value ORDER BY key) FROM json_each(watch_thread.value,'$.context') WHERE value NOT IN (SELECT value FROM json_each(?1))),'[]'))))
	WHERE key IN (SELECT thread_key FROM watch_subscription WHERE work_id` + expiredIDs + ")"

func (t *SQLiteTx) deleteExpired(kind string, ids []string, result *engine.PruneResult) error {
	raw, err := json.Marshal(ids)
	if err != nil {
		return fmt.Errorf("编码过期 %s 标识: %w", kind, err)
	}
	keys := string(raw)
	switch kind {
	case "work":
		if _, err := t.tx.ExecContext(t.ctx, removeSubscriptions, keys); err != nil {
			return fmt.Errorf("清除过期 Work 订阅: %w", err)
		}
	case "trigger":
		result.Dispositions, err = t.pruneCount("DELETE FROM watch_disposition WHERE key"+expiredIDs, keys)
		if err != nil {
			return err
		}
		result.TriggerThreads, err = t.pruneCount("DELETE FROM watch_trigger_thread WHERE trigger_id"+expiredIDs, keys)
		if err != nil {
			return err
		}
	}
	count, err := t.pruneCount("DELETE FROM watch_"+kind+" WHERE key"+expiredIDs, keys)
	switch kind {
	case "work":
		result.Works = count
	case "decision":
		result.Decisions = count
	case "trigger":
		result.Triggers = count
	case "thread":
		result.Threads = count
	}
	return err
}

func (t *SQLiteTx) pruneCount(query string, args ...any) (int, error) {
	res, err := t.tx.ExecContext(t.ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("执行保留期清理: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清理行数: %w", err)
	}
	return int(n), nil
}

func (t *SQLiteTx) pruneEvents(before string) (int, error) {
	// 时间可乱序；最近窗口按 rev 判定，独立于事件自己的 at。
	n, err := t.pruneCount(`DELETE FROM watch_event WHERE value->>'$.at'>'' AND value->>'$.at'<? AND json_extract(value,'$.rev')<
		(SELECT json_extract(value,'$.rev') FROM watch_event ORDER BY json_extract(value,'$.rev') DESC LIMIT 1 OFFSET ?)`, before, engine.MinRetainedEvents-1)
	if err != nil || n == 0 {
		return n, err
	}
	_, err = t.tx.ExecContext(t.ctx, `INSERT INTO watch_kv(key,value)
		SELECT 'event.min_rev',CAST(min(json_extract(value,'$.rev')) AS TEXT) FROM watch_event WHERE true
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return 0, fmt.Errorf("更新事件最小修订: %w", err)
	}
	return n, nil
}
