package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type sqliteRead struct {
	ctx context.Context
	tx  *sql.Tx
}

type sqliteTx struct {
	sqliteRead
	callbacks
}

// Read 从独立只读连接开始事务，同一回调的多次查询共享快照。
func (s *SQLite) Read(ctx context.Context, fn func(ReadTx) error) error {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("开始 SQLite 读事务: %w", err)
	}
	return transact(tx, func(tx *sql.Tx) error {
		// 延迟事务直到首次读取库页才固定快照，须在将控制权交给回调前读取。
		var version int
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("建立 SQLite 读快照: %w", err)
		}
		return fn(sqliteRead{ctx, tx})
	})
}

// Atomic 独占写连接直到提交或回滚，提交回调在释放写锁之后执行。
func (s *SQLite) Atomic(ctx context.Context, fn func(Tx) error) error {
	var write sqliteTx
	err := s.withWriter(ctx, func() error {
		tx, err := s.writer.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("开始 SQLite 写事务: %w", err)
		}
		return transact(tx, func(tx *sql.Tx) error {
			write.sqliteRead = sqliteRead{ctx, tx}
			return fn(&write)
		})
	})
	if err != nil {
		return err
	}
	write.run(s.logger)
	return nil
}

func transact(tx *sql.Tx, fn func(*sql.Tx) error) (err error) {
	defer rollback(tx, &err)
	if err := fn(tx); err != nil {
		return fmt.Errorf("执行 SQLite 事务: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 SQLite 事务: %w", err)
	}
	return nil
}

func rollback(tx *sql.Tx, err *error) {
	if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
		*err = errors.Join(*err, fmt.Errorf("回滚 SQLite 事务: %w", e))
	}
}

func (t *sqliteTx) Atomic(ctx context.Context, fn func(Tx) error) (err error) {
	// SQLite 总是匹配最近的同名保存点；同一 Tx 同步嵌套，无需进程级计数器。
	if _, err := t.tx.ExecContext(ctx, "SAVEPOINT nested"); err != nil {
		return fmt.Errorf("建立保存点: %w", err)
	}
	released := false
	defer func() {
		if released {
			return
		}
		// 即使内层 context 已取消，也要清理保存点，父事务才可继续。
		_, e := t.tx.Exec("ROLLBACK TO nested")
		if e == nil {
			_, e = t.tx.Exec("RELEASE nested")
		}
		if e != nil {
			err = errors.Join(err, fmt.Errorf("回滚并释放保存点: %w", e))
			// 保存点状态已不可信，防止父回调忽略错误后提交部分修改。
			rollback(t.tx, &err)
		}
	}()

	child := sqliteTx{sqliteRead: sqliteRead{ctx, t.tx}}
	if err := fn(&child); err != nil {
		return fmt.Errorf("执行保存点事务: %w", err)
	}
	if _, err := t.tx.ExecContext(ctx, "RELEASE nested"); err != nil {
		return fmt.Errorf("释放保存点: %w", err)
	}
	released = true
	t.callbacks = append(t.callbacks, child.callbacks...)
	return nil
}
