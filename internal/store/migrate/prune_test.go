package migrate

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestPruneMigrationBackfillsReferences(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "old.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Error(err)
		}
	}()
	if err := createV1(tx); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO watch_work(key,value) VALUES('w','{"origin":{"triggerId":"t","triggerIds":["t"]},"inputs":[{"triggerId":"input"}]}')`,
		`INSERT INTO watch_decision VALUES('d','{"workId":"w","triggerIds":["t"]}')`,
		`INSERT INTO watch_effect(kind,key,value,seq,generation,attempts,due,since) VALUES('stop','a','{"workId":"w"}',1,1,0,'','')`,
		"PRAGMA user_version=1",
	} {
		if _, err := tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, conn); err != nil {
		t.Fatal(err)
	}
	for _, ref := range [][4]string{{"work", "w", "trigger", "t"}, {"work", "w", "trigger", "input"}, {"decision", "d", "work", "w"}, {"effect", `["stop","a"]`, "work", "w"}} {
		var count int
		err := conn.QueryRowContext(ctx, `SELECT count(*) FROM watch_prune_ref WHERE source=? AND owner=? AND target=? AND id=?`, ref[0], ref[1], ref[2], ref[3]).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("升级后的引用 %v，共 %d 行，错误 %v", ref, count, err)
		}
	}
}
