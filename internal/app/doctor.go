package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chhuax/craft/internal/store"
)

// Check 是一项自检的结果，Detail 给出实测值或无法完成检查的原因。
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Doctor 在指定环境的状态目录执行自检，依赖不可用时保留未执行的检查项。
func Doctor(ctx context.Context, getenv func(string) string, userHomeDir func() (string, error)) []Check {
	dir, err := resolveStateDir(getenv, userHomeDir)
	state := Check{"stateDir", "ok", dir}
	open := Check{"databaseOpen", "skipped", "状态目录不可用"}
	info := Check{"databaseInfo", "skipped", "数据库未打开"}
	if err == nil {
		err = probeStateDir(dir)
		// 即使无法创建临时文件，也要独立检验现有库能否打开。
		open, info = checkDatabase(ctx, filepath.Join(dir, "craft.db"))
	}
	if err != nil {
		state = Check{"stateDir", "failed", err.Error()}
	}
	return []Check{
		state, open, info,
		{"host", "skipped", "尚未实现"},
		{"extensions", "skipped", "尚未实现"},
	}
}

func probeStateDir(dir string) (err error) {
	file, err := os.CreateTemp(dir, ".craft-doctor-*")
	if err != nil {
		return fmt.Errorf("创建临时文件: %w", err)
	}
	// Windows 要求先关闭再删除；即使关闭出错，仍须尝试清理并报告原因。
	defer func() {
		if e := errors.Join(file.Close(), os.Remove(file.Name())); e != nil {
			err = fmt.Errorf("清理临时文件: %w", e)
		}
	}()
	return nil
}

func checkDatabase(ctx context.Context, path string) (open, info Check) {
	db, err := store.OpenSQLite(ctx, path, nil)
	if err != nil {
		return Check{"databaseOpen", "failed", err.Error()}, Check{"databaseInfo", "skipped", "数据库未打开"}
	}
	defer func() {
		if err := db.Close(); err != nil {
			open = Check{"databaseOpen", "failed", fmt.Sprintf("关闭数据库 %q: %v", path, err)}
		}
	}()

	open = Check{"databaseOpen", "ok", path}
	data, err := db.Info(ctx)
	if err != nil {
		return open, Check{"databaseInfo", "failed", err.Error()}
	}
	detail := fmt.Sprintf("schema=%d, journal_mode=%s, SQLite=%s",
		data.SchemaVersion, data.JournalMode, data.SQLiteVersion)
	info = Check{"databaseInfo", "ok", detail}
	if data.JournalMode != "wal" {
		info.Status = "failed"
		info.Detail += "，需要 journal_mode=wal"
	}
	return open, info
}
