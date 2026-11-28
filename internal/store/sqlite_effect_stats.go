package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// EffectStats 描述该类仍在队中的行。Oldest 是最早的入队时间，LastError 来自最近失败且未被覆盖的行。
type EffectStats struct {
	Waiting, MaxAttempts int
	Oldest, LastError    string
}

const effectStatsQuery = `SELECT waiting,
	(SELECT since FROM watch_effect WHERE kind=c.kind ORDER BY since LIMIT 1),
	coalesce((SELECT attempts FROM watch_effect WHERE kind=c.kind AND attempts>0 ORDER BY attempts DESC LIMIT 1),0),
	coalesce((SELECT error FROM watch_effect WHERE kind=c.kind AND failure_order IS NOT NULL ORDER BY failure_order DESC LIMIT 1),'')
	FROM effect_count c WHERE kind=?`

// EffectStats 读取同一快照中的计数与索引端点，空队列返回零值。
func (r SQLiteReader) EffectStats(kind string) (EffectStats, error) {
	var s EffectStats
	err := r.tx.QueryRowContext(r.ctx, effectStatsQuery, kind).Scan(&s.Waiting, &s.Oldest, &s.MaxAttempts, &s.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return EffectStats{}, nil
	}
	if err != nil {
		return EffectStats{}, fmt.Errorf("读取 %s 的队列统计: %w", kind, err)
	}
	return s, nil
}

const effectNextDue = "SELECT due FROM watch_effect WHERE kind=? AND due>? ORDER BY due,seq LIMIT 1"

// NextEffectDue 返回指定时刻之后的第一个到期时间，无未来行时返回零值。
func (r SQLiteReader) NextEffectDue(kind string, after time.Time) (time.Time, error) {
	var due string
	err := r.tx.QueryRowContext(r.ctx, effectNextDue, kind, after.UTC().Format(milliseconds)).Scan(&due)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("读取 %s 的下一到期时间: %w", kind, err)
	}
	at, err := time.Parse(milliseconds, due)
	if err != nil {
		return time.Time{}, fmt.Errorf("解析 %s 的到期时间 %q: %w", kind, due, err)
	}
	return at, nil
}
