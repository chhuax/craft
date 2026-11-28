package store

import (
	"encoding/json"
	"fmt"
)

// PutState 在写事务内按命名空间限制新键数量，已存在的键即使达到上限也能覆盖。
func (t *SQLiteTx) PutState(namespace, key string, value json.RawMessage, limit int) (bool, error) {
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO ext_state(namespace,key,value)
	SELECT ?1,?2,?3 WHERE EXISTS(SELECT 1 FROM ext_state WHERE namespace=?1 AND key=?2)
	OR (SELECT count(*) FROM ext_state WHERE namespace=?1)<?4
	ON CONFLICT(namespace,key) DO UPDATE SET value=excluded.value`, namespace, key, string(value), limit)
	if err != nil {
		return false, fmt.Errorf("写入扩展 %q 的状态: %w", namespace, err)
	}
	n, err := res.RowsAffected()
	return n != 0, err
}
