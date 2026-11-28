package app

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/engine/enginetest"
	"github.com/chhuax/craft/internal/engine/repotest"
	"github.com/chhuax/craft/internal/store"
)

func sqliteRepository(t testing.TB) engine.Store {
	t.Helper()
	db, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "engine.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return NewSQLiteStore(db)
}

func TestSQLiteRepository(t *testing.T) {
	repotest.Run(t, func(t *testing.T) engine.Store { return sqliteRepository(t) })
}

func TestSQLiteEngine(t *testing.T) {
	enginetest.Run(t, func(t *testing.T) engine.Store { return sqliteRepository(t) })
}

func TestSQLiteReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reopen.db")
	open := func(t *testing.T) *store.SQLite {
		t.Helper()
		db, err := store.OpenSQLite(t.Context(), path, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		return db
	}
	t.Run("首次提交", func(t *testing.T) {
		s := NewSQLiteStore(open(t))
		err := s.Atomic(ctx, func(u engine.Unit) error {
			invalid := engine.Event{Data: map[string]json.RawMessage{"bad": json.RawMessage("NaN")}}
			if err := u.Emit(invalid); err == nil {
				t.Fatal("无效事件写入成功")
			}
			for range 11 {
				if err := u.Emit(engine.Event{Type: "work.changed"}); err != nil {
					return err
				}
			}
			return u.Effects().Enqueue("delivery", "w", json.RawMessage("1"))
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	db := open(t)
	s := NewSQLiteStore(db)
	err := s.Atomic(ctx, func(u engine.Unit) error {
		if err := u.Effects().Enqueue("delivery", "w", json.RawMessage("2")); err != nil {
			return err
		}
		return u.Emit(engine.Event{Type: "work.changed"})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Read(ctx, func(r engine.Reader) error {
		rows, err := r.EventsSince(9, 5)
		if err == nil && (len(rows) != 3 || rows[0].Rev != 10 || rows[1].Rev != 11 || rows[2].Rev != 12) {
			t.Fatalf("重新打开后的补拉页 %+v", rows)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.View(ctx, func(r store.SQLiteReader) error {
		row, err := r.HeadEffect("delivery")
		if err == nil && (row == nil || string(row.Value) != "2" || row.Seq != 1 || row.Generation != 2) {
			t.Fatalf("重新打开后的队首 %+v", row)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
