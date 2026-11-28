package store

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func openTestDB(t *testing.T, path string) *SQLite {
	t.Helper()
	s, err := OpenSQLite(t.Context(), path, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestSQLiteVersion(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "version.db"))
	var version string
	if err := s.reader.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		t.Fatalf("未知 SQLite 版本 %q", version)
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatal(err)
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || major < 3 || (major == 3 && minor < 31) {
		t.Fatalf("SQLite %s，需要 3.31 或更高版本", version)
	}

	if _, err := s.writer.Exec(`CREATE TABLE probe (value TEXT, last TEXT AS (json_extract(value, '$[#-1]')))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.Exec(`INSERT INTO probe (value) VALUES ('["first","last"]')`); err != nil {
		t.Fatal(err)
	}
	var last string
	if err := s.reader.QueryRow("SELECT last FROM probe").Scan(&last); err != nil || last != "last" {
		t.Fatalf("生成列取末项 = %q, %v", last, err)
	}
}

func TestSchemaV1(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "schema.db"))
	var version int
	if err := s.reader.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 1 {
		t.Fatalf("user_version = %d, %v", version, err)
	}
	for table, want := range map[string][]string{
		"watch_inbox":  {"id", "value", "at", "seq"},
		"watch_effect": {"kind", "key", "value", "seq", "generation", "attempts", "error", "due", "since"},
		"alarm_state":  {"key", "value"},
		"ext_state":    {"key", "value"},
	} {
		t.Run(table, func(t *testing.T) {
			rows, err := s.reader.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var got []string
			for rows.Next() {
				var col string
				if err := rows.Scan(&col); err != nil {
					t.Fatal(err)
				}
				got = append(got, col)
			}
			if err := rows.Err(); err != nil || !slices.Equal(got, want) {
				t.Fatalf("列 = %v, %v，期望 %v", got, err, want)
			}
		})
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "newer.db")
	s := openTestDB(t, path)
	if _, err := s.writer.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	other, err := OpenSQLite(t.Context(), path, nil)
	if err == nil {
		t.Cleanup(func() {
			if err := other.Close(); err != nil {
				t.Error(err)
			}
		})
		t.Fatal("打开了较新版本的库")
	}
	if !strings.Contains(err.Error(), "2") || !strings.Contains(err.Error(), "1") {
		t.Fatalf("错误缺少库版本或程序版本：%v", err)
	}
	var version int
	if err := s.reader.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("拒绝打开后 user_version = %d, %v", version, err)
	}
}

func TestMigrationRollsBackDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// 在迁移的最后一张表制造冲突，先前已执行的 DDL 和版本号必须一起回滚。
	if _, err := db.Exec("CREATE TABLE ext_state (old TEXT)"); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenSQLite(t.Context(), path, nil); err == nil {
		defer s.Close()
		t.Fatal("存在表冲突时仍然完成了迁移")
	}
	var tables, version int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type = 'table'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || tables != 1 || version != 0 {
		t.Fatalf("失败后表数 = %d，版本 = %d，错误 = %v", tables, version, err)
	}
	if _, err := db.Exec("DROP TABLE ext_state"); err != nil {
		t.Fatal(err)
	}
	openTestDB(t, path)
}

func TestSQLiteConnections(t *testing.T) {
	// 文件名里的 URI 特殊字符必须原样保留，不能成为连接参数。
	path := filepath.Join(t.TempDir(), "连接 #%.db")
	s := openTestDB(t, path)
	for _, db := range []*sql.DB{s.writer, s.reader} {
		checkConnection(t, db)
	}
	conn, err := s.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// 占用第一条读连接，迫使连接池为第二个使用者创建新的连接。
	checkConnection(t, s.reader)
	if _, err := conn.ExecContext(t.Context(), "DELETE FROM watch_kv"); err == nil {
		t.Fatal("只读连接接受了写入")
	}
	other := openTestDB(t, path)
	version := func() int64 {
		t.Helper()
		v, err := s.DataVersion(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := version()
	if _, err := s.writer.Exec("INSERT INTO watch_kv VALUES ('local','1')"); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != before {
		t.Fatalf("自己的写入改变了 data_version: %d -> %d", before, v)
	}
	if _, err := other.writer.Exec("INSERT INTO watch_kv VALUES ('other','1')"); err != nil {
		t.Fatal(err)
	}
	if v := version(); v == before {
		t.Fatal("未发现其他连接的写入")
	}
}

func checkConnection(t *testing.T, db *sql.DB) {
	t.Helper()
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for name, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "2", "busy_timeout": "5000", "foreign_keys": "1",
	} {
		var got string
		if err := conn.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q, %v，期望 %q", name, got, err, want)
		}
	}
}
