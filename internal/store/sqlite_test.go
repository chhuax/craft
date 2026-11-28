package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

func openTestDB(t testing.TB, path string) *SQLite {
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

// TestPutManySQLiteJSON 验证数据库拒绝文档时，外层事务仍能提交先前的修改。
func TestPutManySQLiteJSON(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "depth.db"))
	// 这份 JSON 合乎语法，但嵌套层数超过了 SQLite JSON 解析器的上限。
	deep := json.RawMessage(strings.Repeat("[", 1001) + "0" + strings.Repeat("]", 1001))
	const table = "watch_kv"
	err := s.Atomic(t.Context(), func(tx Tx) error {
		if err := tx.Put(table, "existing", json.RawMessage("1")); err != nil {
			return err
		}
		err := tx.PutMany(table, []Doc{
			{Key: "existing", Value: json.RawMessage("2")},
			{Key: "deep", Value: deep},
		})
		if err == nil {
			t.Fatal("SQLite 接受了嵌套过深的 JSON")
		}
		raw, found, err := tx.Get(table, "existing")
		if err != nil {
			return err
		}
		if !found || string(raw) != "1" {
			t.Fatalf("批量写入失败后原文档 = %s, %v", raw, found)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
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

	if _, err := s.writer.ExecContext(t.Context(), `CREATE TABLE probe (value TEXT, last TEXT AS (json_extract(value, '$[#-1]')))`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.writer.ExecContext(t.Context(), `INSERT INTO probe (value) VALUES ('["first","last"]')`); err != nil {
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
		"watch_thread":         {"ordinal", "key", "value", "ready", "eval_id", "closed"},
		"watch_work":           {"ordinal", "key", "value", "created", "origin_id", "origin_thread", "ended"},
		"watch_work_input":     {"work_key", "input_id", "position"},
		"watch_reply_status":   {"key", "value"},
		"watch_subscription":   {"thread_key", "work_id"},
		"watch_acceptance":     {"decision_id", "trigger_id", "work_id"},
		"watch_trigger":        {"key", "value", "primary_thread"},
		"watch_trigger_thread": {"key", "value", "trigger_id", "thread_key"},
		"watch_decision":       {"key", "value", "thread_key"},
		"watch_inbox":          {"id", "value", "at", "seq"},
		"watch_effect":         {"kind", "key", "value", "seq", "generation", "attempts", "error", "due", "since", "failure_order"},
		"document_count":       {"name", "total"},
		"effect_count":         {"kind", "waiting"},
		"outbox":               {"key", "value", "state", "auto_at"},
		"alarm_state":          {"key", "value"},
		"ext_state":            {"namespace", "key", "value"},
		"watch_alarm_request":  {"ordinal", "key", "value", "status"},
	} {
		t.Run(table, func(t *testing.T) {
			rows, err := s.reader.Query("SELECT name FROM pragma_table_xinfo(?) ORDER BY cid", table)
			if err != nil {
				t.Fatal(err)
			}
			defer closeTest(t, rows)
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
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, db)
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
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
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatalf("拒绝打开后 user_version = %d, %v", version, err)
	}
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "delete" {
		t.Fatalf("拒绝打开后 journal_mode = %q, %v", mode, err)
	}
}

func TestMigrationRollsBackDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, db)
	// 在迁移的最后一张表制造冲突，先前已执行的 DDL 和版本号必须一起回滚。
	if _, err := db.Exec("CREATE TABLE ext_state (old TEXT)"); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenSQLite(t.Context(), path, nil); err == nil {
		defer closeTest(t, s)
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
	checkConnection(t, s.writer)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	conn, err := s.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, conn)
	checkConnection(t, conn)
	// 占用第一条读连接，迫使连接池为第二个使用者创建新的连接。
	second, err := s.reader.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer closeTest(t, second)
	checkConnection(t, second)
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
	if _, err := s.writer.ExecContext(t.Context(), "INSERT INTO watch_kv VALUES ('local','1')"); err != nil {
		t.Fatal(err)
	}
	if v := version(); v != before {
		t.Fatalf("自己的写入改变了 data_version: %d -> %d", before, v)
	}
	if _, err := other.writer.ExecContext(t.Context(), "INSERT INTO watch_kv VALUES ('other','1')"); err != nil {
		t.Fatal(err)
	}
	if v := version(); v == before {
		t.Fatal("未发现其他连接的写入")
	}
}

func closeTest(t *testing.T, resource io.Closer) {
	t.Helper()
	if err := resource.Close(); err != nil {
		t.Error(err)
	}
}

func checkConnection(t *testing.T, conn *sql.Conn) {
	t.Helper()
	for name, want := range map[string]string{
		"journal_mode": "wal", "synchronous": "2", "busy_timeout": "5000", "foreign_keys": "1",
	} {
		var got string
		if err := conn.QueryRowContext(t.Context(), "PRAGMA "+name).Scan(&got); err != nil || got != want {
			t.Errorf("%s = %q, %v，期望 %q", name, got, err, want)
		}
	}
}

func TestImmediateWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writers.db")
	first := openTestDB(t, path)
	second := openTestDB(t, path)
	ctx := t.Context()
	// 第二个实例立即报告锁冲突，测试无需等候 busy_timeout 到期。
	if _, err := second.writer.ExecContext(ctx, "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}
	err := first.Atomic(ctx, func(Tx) error {
		// 首个事务还未读写文档，此时已经持有写锁才能证明使用了 IMMEDIATE。
		err := second.Atomic(ctx, func(Tx) error { return nil })
		var sqliteErr *sqlite.Error
		const sqliteBusy = 5
		if !errors.As(err, &sqliteErr) || sqliteErr.Code() != sqliteBusy {
			t.Fatalf("第二个写者应收到 SQLITE_BUSY，实际为 %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Atomic(ctx, func(Tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestActiveIndexes(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "indexes.db"))
	for table, field := range map[string]string{"watch_thread": "closed", "watch_work": "ended"} {
		var id, parent, unused int
		var detail string
		plan := "EXPLAIN QUERY PLAN SELECT key, value FROM " + table + " WHERE " + field + " IS NULL"
		if err := s.reader.QueryRow(plan).Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(detail, table+"_active") {
			t.Errorf("%s 的活跃集未使用部分索引：%s", table, detail)
		}
	}
}

// BenchmarkPutMany 比较同一批 1000 份文档逐条提交与整批提交到 SQLite 文件的耗时。
func BenchmarkPutMany(b *testing.B) {
	const collection = "watch_kv"
	docs := make([]Doc, 1000)
	for i := range docs {
		docs[i] = Doc{Key: strconv.Itoa(i), Value: json.RawMessage(`{"active":true}`)}
	}
	ctx := b.Context()
	run := func(name string, write func(*SQLite) error) {
		b.Run(name, func(b *testing.B) {
			s := openTestDB(b, filepath.Join(b.TempDir(), "bench.db"))
			for b.Loop() {
				// 在计时外清表，确保每轮都实际插入 1000 行。
				b.StopTimer()
				if _, err := s.writer.ExecContext(ctx, "DELETE FROM watch_kv"); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if err := write(s); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	run("Put", func(s *SQLite) error {
		for _, doc := range docs {
			if err := s.Atomic(ctx, func(tx Tx) error {
				return tx.Put(collection, doc.Key, doc.Value)
			}); err != nil {
				return err
			}
		}
		return nil
	})
	run("PutMany", func(s *SQLite) error {
		return s.Atomic(ctx, func(tx Tx) error { return tx.PutMany(collection, docs) })
	})
}
