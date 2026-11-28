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

const readVersion = "PRAGMA user_version"

func supportedVersion(row *sql.Row) (int, error) {
	var current int
	if err := row.Scan(&current); err != nil {
		return 0, fmt.Errorf("读取库版本: %w", err)
	}
	latest := migrations[len(migrations)-1].Version
	if current > latest {
		return 0, fmt.Errorf("数据库版本 %d 高于程序支持的版本 %d，拒绝打开", current, latest)
	}
	return current, nil
}

// Apply 检查兼容性后启用 WAL 并迁移，要求 writer 使用驱动的 immediate 事务模式。
func Apply(ctx context.Context, writer *sql.Conn) (err error) {
	// 切换 WAL 会持久修改文件且只能在事务外执行，先保护较新格式的库。
	if _, err := supportedVersion(writer.QueryRowContext(ctx, readVersion)); err != nil {
		return err
	}
	var mode string
	if err := writer.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return fmt.Errorf("启用 WAL: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("无法启用 WAL，实际 journal_mode 为 %q", mode)
	}
	tx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始迁移事务: %w", err)
	}
	defer func() {
		if e := tx.Rollback(); e != nil && !errors.Is(e, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("回滚迁移: %w", e))
		}
	}()

	// 取得写锁后重读，另一个进程可能先完成了迁移。
	current, err := supportedVersion(tx.QueryRowContext(ctx, readVersion))
	if err != nil {
		return err
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
