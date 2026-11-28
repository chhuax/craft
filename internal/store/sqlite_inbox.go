package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// InboxRow 保留队列中的原始文档及入队顺序，文档内容由调用方解释。
type InboxRow struct {
	ID    string
	Value json.RawMessage
	At    string
	Seq   int64
}

const (
	inboxBatch  = "SELECT id,value,at,seq FROM watch_inbox ORDER BY seq LIMIT ?"
	inboxHead   = "SELECT seq FROM watch_inbox ORDER BY seq LIMIT 1"
	inboxDelete = "DELETE FROM watch_inbox WHERE id=?"
)

// InsertInbox 分配事务内序号，仅首次插入返回 true。调用方须将错误传回写事务。
func (t *SQLiteTx) InsertInbox(id string, value json.RawMessage, at string) (bool, error) {
	if err := validateJSON("watch_inbox", id, value); err != nil {
		return false, err
	}
	seq, err := t.NextCounter("inbox.seq")
	if err != nil {
		return false, err
	}
	result, err := t.tx.ExecContext(t.ctx, "INSERT OR IGNORE INTO watch_inbox(id,value,at,seq) VALUES (?,?,?,?)", id, string(value), at, seq)
	if err != nil {
		return false, fmt.Errorf("写入 inbox[%q]: %w", id, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取 inbox[%q] 插入行数: %w", id, err)
	}
	return n == 1, nil
}

// HasInbox 只读 seq 索引的第一项，让空闲的 Pass 不必取得写锁。
func (r SQLiteReader) HasInbox() (bool, error) {
	var seq int64
	err := r.tx.QueryRowContext(r.ctx, inboxHead).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("检查 inbox 队首: %w", err)
	}
	return true, nil
}

// InboxBatch 按序号返回至多 limit 条记录；消费方须在写事务内选取和删除。
func (r SQLiteReader) InboxBatch(limit int) (_ []InboxRow, err error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.tx.QueryContext(r.ctx, inboxBatch, limit)
	if err != nil {
		return nil, fmt.Errorf("查询 inbox: %w", err)
	}
	defer func() { err = errors.Join(err, rows.Close()) }()
	var batch []InboxRow
	for rows.Next() {
		var row InboxRow
		var raw []byte
		if err := rows.Scan(&row.ID, &raw, &row.At, &row.Seq); err != nil {
			return nil, fmt.Errorf("读取 inbox 记录: %w", err)
		}
		row.Value = raw
		batch = append(batch, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 inbox: %w", err)
	}
	return batch, nil
}

// DeleteInbox 按主键移除记录，须与对该行的处理一起提交。
func (t *SQLiteTx) DeleteInbox(id string) error {
	if _, err := t.tx.ExecContext(t.ctx, inboxDelete, id); err != nil {
		return fmt.Errorf("删除 inbox[%q]: %w", id, err)
	}
	return nil
}
