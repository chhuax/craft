package store

import (
	"context"
	"fmt"
)

// Info 描述当前数据库的 schema、日志模式和驱动使用的 SQLite 版本。
type Info struct {
	SchemaVersion int
	JournalMode   string
	SQLiteVersion string
}

// Info 从已打开的库读取诊断信息，无需占用写连接。
func (s *SQLite) Info(ctx context.Context) (Info, error) {
	var info Info
	if err := s.reader.QueryRowContext(ctx, "PRAGMA user_version").Scan(&info.SchemaVersion); err != nil {
		return Info{}, fmt.Errorf("读取 user_version: %w", err)
	}
	if err := s.reader.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&info.JournalMode); err != nil {
		return Info{}, fmt.Errorf("读取 journal_mode: %w", err)
	}
	if err := s.reader.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&info.SQLiteVersion); err != nil {
		return Info{}, fmt.Errorf("读取 SQLite 版本: %w", err)
	}
	return info, nil
}
