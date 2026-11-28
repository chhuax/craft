package app

import (
	"encoding/json"
	"path/filepath"
	"slices"
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

// TestSQLiteDeliveryReplacement 覆盖整份替换导致输入换位，以及调用方捕获回写错误后仍然提交的情形。
func TestSQLiteDeliveryReplacement(t *testing.T) {
	ctx := t.Context()
	s := sqliteRepository(t)
	a := engine.WorkInput{ID: "same", Kind: "trigger", TriggerID: "first"}
	b := engine.WorkInput{ID: "same", Kind: "trigger", TriggerID: "last"}
	w := engine.Work{ID: "w", Assignments: []engine.Assignment{{ID: "a"}}, Undelivered: []string{"same"}, Inputs: []engine.WorkInput{a, b}}
	err := s.Atomic(ctx, func(u engine.Unit) error {
		if err := u.PutWork(w); err != nil {
			return err
		}
		window, err := u.GetDeliveryWindow(w.ID, []string{"same"})
		if err != nil {
			return err
		}
		if window.Inputs["same"].TriggerID != "last" {
			t.Fatal("重复 ID 未定位到最后一项")
		}
		w.Inputs = []engine.WorkInput{b, a}
		w.Undelivered = []string{"same", "tail"}
		if err := u.PutWork(w); err != nil {
			return err
		}
		in := window.Inputs["same"]
		in.ReadAt = engine.ReadTime{Present: true}
		window.Inputs["same"] = in
		if err := u.PutDeliveryWindow(w.ID, *window, 1); err != nil {
			return err
		}
		if err := u.PutDeliveryWindow(w.ID, *window, 1); err == nil {
			t.Fatal("接受了旧队首")
		}
		// 在输入与队首已写入后让最后一步编码失败；父事务仍正常提交。
		window.Head = []string{"tail"}
		window.UpdatedAt = string([]byte{0xff})
		in.DeliveredTo = "未提交"
		window.Inputs["same"] = in
		if err := u.PutDeliveryWindow(w.ID, *window, 1); err == nil {
			t.Fatal("接受了非法更新时间")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Read(ctx, func(r engine.Reader) error {
		got, err := r.GetWork(w.ID)
		if err == nil && (!slices.Equal(got.Undelivered, []string{"tail"}) || got.Inputs[0].ReadAt.Present ||
			!got.Inputs[1].ReadAt.Present || got.Inputs[1].TriggerID != "first" || got.Inputs[1].DeliveredTo != "") {
			t.Fatalf("回执改动了旧位置或回滚不完整 %+v", got)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
