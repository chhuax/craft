package store

import (
	"path/filepath"
	"testing"
)

func TestSQLiteInfo(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "info.db"))
	// 区分文件中的实际版本和程序内置的迁移版本。
	if _, err := db.writer.ExecContext(t.Context(), "PRAGMA user_version = 42"); err != nil {
		t.Fatal(err)
	}
	info, err := db.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if info.SchemaVersion != 42 || info.JournalMode != "wal" || info.SQLiteVersion == "" {
		t.Errorf("库信息 = %+v", info)
	}
}
