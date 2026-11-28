// Package migrate 管理 SQLite 文件的版本，使旧程序在写入新格式前就能发现不兼容。
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Migration 的版本递增，Up 只在调用方持有写锁的事务中修改 schema。
type Migration struct {
	Version int
	Up      func(*sql.Tx) error
}

var migrations = []Migration{{Version: 1, Up: createV1}}

// Apply 要求 writer 使用驱动的 immediate 事务模式，防止两个进程同时按旧版本迁移。
func Apply(ctx context.Context, writer *sql.DB) (err error) {
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移事务: %w", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("回滚迁移: %w", e))
		}
	}()

	var current int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("读取库版本: %w", err)
	}
	latest := migrations[len(migrations)-1].Version
	if current > latest {
		return fmt.Errorf("数据库版本 %d 高于程序支持的版本 %d，拒绝打开", current, latest)
	}
	for _, m := range migrations {
		if m.Version <= current {
			continue
		}
		if err := m.Up(tx); err != nil {
			return fmt.Errorf("迁移到版本 %d: %w", m.Version, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
			return fmt.Errorf("记录库版本 %d: %w", m.Version, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交迁移: %w", err)
	}
	return nil
}
