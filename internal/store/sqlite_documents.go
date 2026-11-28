package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chhuax/craft/internal/store/migrate"
)

func (r sqliteRead) Get(collection, key string) (json.RawMessage, bool, error) {
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return nil, false, err
	}
	var value []byte
	err = r.tx.QueryRowContext(r.ctx, "SELECT value FROM "+table+" WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("读取 %s[%q]: %w", table, key, err)
	}
	return value, true, nil
}

func (r sqliteRead) List(collection, filter string) (_ map[string]json.RawMessage, err error) {
	table, missing, err := migrate.Resolve(collection, filter)
	if err != nil {
		return nil, err
	}
	query := "SELECT key, value FROM " + table
	if missing != "" {
		query += " WHERE " + missing + " IS NULL"
	}
	rows, err := r.tx.QueryContext(r.ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询集合 %s: %w", table, err)
	}
	defer func() {
		if e := rows.Close(); e != nil {
			err = errors.Join(err, fmt.Errorf("关闭集合 %s 的查询: %w", table, e))
		}
	}()

	result := make(map[string]json.RawMessage)
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("读取集合 %s 的文档: %w", table, err)
		}
		result[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历集合 %s: %w", table, err)
	}
	return result, nil
}

func putQuery(table string) string {
	return "INSERT INTO " + table + " (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value"
}

func (t *sqliteTx) Put(collection, key string, value json.RawMessage) error {
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return err
	}
	if err := validateJSON(collection, key, value); err != nil {
		return err
	}
	// 显式绑定 TEXT，直接传 RawMessage 会绑定为 BLOB 并改变 SQLite 的 JSON 解析语义。
	if _, err := t.tx.ExecContext(t.ctx, putQuery(table), key, string(value)); err != nil {
		return fmt.Errorf("写入 %s[%q]: %w", table, key, err)
	}
	return nil
}

func (t *sqliteTx) PutMany(collection string, docs []Doc) (err error) {
	if len(docs) == 0 {
		return nil
	}
	table, err := validateDocs(collection, docs)
	if err != nil {
		return err
	}
	stmt, err := t.tx.PrepareContext(t.ctx, putQuery(table))
	if err != nil {
		return fmt.Errorf("准备批量写入 %s: %w", table, err)
	}
	defer func() {
		if e := stmt.Close(); e != nil {
			err = errors.Join(err, fmt.Errorf("关闭集合 %s 的写入语句: %w", table, e))
		}
	}()

	// SQLite 还会拒绝嵌套过深等通过 Go 校验的 JSON。用保存点撤销此前的写入，
	// 以便调用方处理错误后仍可提交同一事务内的其他修改。
	return t.Atomic(t.ctx, func(Tx) error {
		for _, doc := range docs {
			if _, err := stmt.ExecContext(t.ctx, doc.Key, string(doc.Value)); err != nil {
				return fmt.Errorf("写入 %s[%q]: %w", table, doc.Key, err)
			}
		}
		return nil
	})
}

func (t *sqliteTx) Delete(collection, key string) error {
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(t.ctx, "DELETE FROM "+table+" WHERE key = ?", key); err != nil {
		return fmt.Errorf("删除 %s[%q]: %w", table, key, err)
	}
	return nil
}
