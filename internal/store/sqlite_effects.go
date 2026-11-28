package store

import (
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"time"
)

// NextCounter 在当前写事务分配序号。耗尽或损坏须报错，以免误用复用的序号。
func (t *SQLiteTx) NextCounter(key string) (int64, error) {
	const query = `INSERT INTO watch_kv(key,value) VALUES (?,'1') ON CONFLICT(key) DO UPDATE
		SET value=CAST(value AS INTEGER)+1 WHERE json_type(value)='integer'
		AND CAST(value AS INTEGER) BETWEEN 0 AND 9223372036854775806 RETURNING value`
	var n int64
	if err := t.tx.QueryRowContext(t.ctx, query, key).Scan(&n); err != nil {
		return 0, fmt.Errorf("递增计数器 %q: %w", key, err)
	}
	return n, nil
}

// Effect 是队列行的读取快照，写回时携带原快照以核对身份与代次。
type Effect struct {
	Kind, Key         string
	Value             json.RawMessage
	Seq, Generation   int64
	Attempts          int
	Error, Due, Since string
}

// Enqueue 与聚合共用当前事务。覆盖保留最初排队时间与位置，新代次可立即执行。
func (t *SQLiteTx) Enqueue(kind, key string, value json.RawMessage) error {
	if !jsontext.Value(value).IsValid() {
		return fmt.Errorf("effect %s[%q] 的负载不是有效 JSON", kind, key)
	}
	const query = `INSERT INTO watch_effect(kind,key,value,seq,generation,attempts,due,since)
		VALUES (?,?,?,?,1,0,?,?)
		ON CONFLICT(kind,key) DO UPDATE SET value=excluded.value,generation=generation+1,
		attempts=0,error=NULL,due=excluded.due`
	// SQLite 对 JSON 深度还有额外限制。即使调用方捕获入队错误也不能只提交计数器。
	return t.Savepoint(t.ctx, func(child *SQLiteTx) error {
		seq, err := child.NextCounter("effect.seq")
		if err != nil {
			return err
		}
		// 时间来自注入的时钟而不是 SQLite 的 'now'，见 WithClock。
		at := child.now().UTC().Format(milliseconds)
		if _, err := child.tx.ExecContext(child.ctx, query, kind, key, string(value), seq, at, at); err != nil {
			return fmt.Errorf("入队 %s[%q]: %w", kind, key, err)
		}
		return nil
	})
}

const effectColumns = "SELECT kind,key,value,seq,generation,attempts,coalesce(error,''),due,since FROM watch_effect "
const effectHead = effectColumns + "WHERE kind=? ORDER BY seq LIMIT 1"
const effectDue = effectColumns + "WHERE kind=? AND due<=? ORDER BY due,seq LIMIT ?"
const milliseconds = "2006-01-02T15:04:05.000Z"

// HeadEffect 读取该类队首，即使它正在退避也不能越过它。
func (r SQLiteReader) HeadEffect(kind string) (*Effect, error) {
	rows, err := r.effects(effectHead, kind)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// DueEffects 读取已到期的至多 limit 行，同一到期时间内按 seq 排队。
func (r SQLiteReader) DueEffects(kind string, now time.Time, limit int) ([]Effect, error) {
	if limit <= 0 {
		return nil, nil
	}
	return r.effects(effectDue, kind, now.UTC().Format(milliseconds), limit)
}

func (r SQLiteReader) effects(query string, args ...any) (_ []Effect, err error) {
	rows, err := r.tx.QueryContext(r.ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 effect 队列: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var result []Effect
	for rows.Next() {
		var e Effect
		var raw []byte
		if err := rows.Scan(&e.Kind, &e.Key, &raw, &e.Seq, &e.Generation, &e.Attempts, &e.Error, &e.Due, &e.Since); err != nil {
			return nil, fmt.Errorf("读取 effect: %w", err)
		}
		e.Value = raw
		result = append(result, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 effect: %w", err)
	}
	return result, nil
}

const sameEffect = " WHERE kind=? AND key=? AND seq=? AND generation=?"

// DeleteEffect 只消费读到的那一代，期间已覆盖或重入队均为空操作。
func (t *SQLiteTx) DeleteEffect(e Effect) error {
	return t.finishEffect(e, "DELETE FROM watch_effect")
}

// LaterEffect 推迟到 now + base，不改变失败记录。
func (t *SQLiteTx) LaterEffect(e Effect, now time.Time, base time.Duration) error {
	return t.finishEffect(e, "UPDATE watch_effect SET due=?", now.Add(base).UTC().Format(milliseconds))
}

// FailEffect 按写回前的失败次数计算退避，到达 ceiling 后不再增长。
func (t *SQLiteTx) FailEffect(e Effect, now time.Time, base, ceiling time.Duration, message string) error {
	// 以毫秒计算，duration 的毫秒数乘以 2^16 仍在 int64 范围内。
	const query = `UPDATE watch_effect SET attempts=attempts+1,error=?,
		due=strftime('%Y-%m-%dT%H:%M:%fZ',?,printf('%+.3f seconds',min(?,?*(1<<min(attempts,16)))/1000.0))`
	n := 0
	for i := range message {
		if n == 1000 {
			message = message[:i]
			break
		}
		n++
	}
	return t.finishEffect(e, query, message, now.UTC().Format(milliseconds), ceiling.Milliseconds(), base.Milliseconds())
}

func (t *SQLiteTx) finishEffect(e Effect, query string, args ...any) error {
	args = append(args, e.Kind, e.Key, e.Seq, e.Generation)
	if _, err := t.tx.ExecContext(t.ctx, query+sameEffect, args...); err != nil {
		return fmt.Errorf("写回 effect %s[%q]: %w", e.Kind, e.Key, err)
	}
	return nil
}
