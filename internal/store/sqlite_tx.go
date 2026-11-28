package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SQLiteReader 只在当前回调内使用，文档和索引共享同一快照。
type SQLiteReader struct {
	ctx context.Context
	tx  *sql.Tx
}

// SQLiteTx 在同一写事务内组合文档、计数器与队列操作。
type SQLiteTx struct {
	SQLiteReader
	callbacks
	now func() time.Time
}

// Read 从独立只读连接开始事务，同一回调的多次查询共享快照。
func (s *SQLite) Read(ctx context.Context, fn func(ReadTx) error) error {
	return s.View(ctx, func(r SQLiteReader) error { return fn(r) })
}

// View 在独立只读连接上提供 SQLite 的索引与队列读取能力。
func (s *SQLite) View(ctx context.Context, fn func(SQLiteReader) error) error {
	tx, err := s.reader.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("开始 SQLite 读事务: %w", err)
	}
	return transact(tx, func(tx *sql.Tx) error {
		// 延迟事务直到首次读取库页才固定快照，须在将控制权交给回调前读取。
		if err := checkVersion(tx.QueryRowContext(ctx, "PRAGMA user_version")); err != nil {
			return fmt.Errorf("建立 SQLite 读快照: %w", err)
		}
		return fn(SQLiteReader{ctx, tx})
	})
}

func checkVersion(row *sql.Row) error {
	var version int
	if err := row.Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本: %w", err)
	}
	if version != 1 {
		return fmt.Errorf("不支持数据库版本 %d", version)
	}
	return nil
}

// Atomic 独占写连接直到提交或回滚，提交回调在释放写锁之后执行。
func (s *SQLite) Atomic(ctx context.Context, fn func(Tx) error) error {
	return s.Write(ctx, func(t *SQLiteTx) error { return fn(t) })
}

// Write 在一个 IMMEDIATE 事务中提供文档和专用表的原子操作。
func (s *SQLite) Write(ctx context.Context, fn func(*SQLiteTx) error) error {
	return s.WriteAndPublish(ctx, fn, nil)
}

// WriteAndPublish 在提交成功后、让出本连接前发布内存快照，避免下一条命令读到旧版。
// publish 只能更新进程内存，不得阻塞或重入仓储；一般通知仍应使用 AfterCommit。
func (s *SQLite) WriteAndPublish(ctx context.Context, fn func(*SQLiteTx) error, publish func()) error {
	var write SQLiteTx
	err := s.withWriter(ctx, func() error {
		tx, err := s.writer.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("开始 SQLite 写事务: %w", err)
		}
		err = transact(tx, func(tx *sql.Tx) error {
			write.SQLiteReader = SQLiteReader{ctx, tx}
			write.now = s.now
			return fn(&write)
		})
		if err == nil && publish != nil {
			publish()
		}
		return err
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

// Atomic 将通用文档事务嵌套在当前保存点内。
func (t *SQLiteTx) Atomic(ctx context.Context, fn func(Tx) error) error {
	return t.Savepoint(ctx, func(child *SQLiteTx) error { return fn(child) })
}

// Savepoint 使内层的索引、计数器和提交回调能与文档一起撤销。
func (t *SQLiteTx) Savepoint(ctx context.Context, fn func(*SQLiteTx) error) (err error) {
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

	child := SQLiteTx{SQLiteReader: SQLiteReader{ctx, t.tx}, now: t.now}
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
