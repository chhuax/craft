package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func requireStoreOK(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func head(t *testing.T, db *SQLite) Effect {
	t.Helper()
	var got Effect
	requireStoreOK(t, db.View(t.Context(), func(r SQLiteReader) error {
		row, err := r.HeadEffect("delivery")
		if err != nil {
			return err
		}
		if row == nil {
			t.Fatal("缺少队首")
		}
		got = *row
		return nil
	}))
	return got
}

func TestEffectGeneration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "queue.db")
	db := openTestDB(t, path)
	other := openTestDB(t, path)
	write := func(db *SQLite, fn func(*SQLiteTx) error) { t.Helper(); requireStoreOK(t, db.Write(ctx, fn)) }
	enqueue := func(tx *SQLiteTx) error { return tx.Enqueue("delivery", "one", json.RawMessage("1")) }
	abort := errors.New("撤销")
	err := db.Write(ctx, func(tx *SQLiteTx) error {
		requireStoreOK(t, enqueue(tx))
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	write(db, enqueue)
	first := head(t, other)
	if first.Seq != 1 || first.Attempts != 0 || first.Since != first.Due {
		t.Fatalf("初次入队 %+v", first)
	}
	write(other, func(tx *SQLiteTx) error {
		requireStoreOK(t, tx.Enqueue("delivery", "two", json.RawMessage("2")))
		return tx.Enqueue("delivery", "one", json.RawMessage("3"))
	})
	merged := head(t, db)
	if string(merged.Value) != "3" || merged.Key != "one" || merged.Seq != first.Seq || merged.Generation != first.Generation+1 || merged.Since != first.Since {
		t.Fatalf("覆盖后的队首 %+v", merged)
	}

	// 分别制造代次过期和删除后同键同代重入队，三种旧写回都必须无效。
	stale := func(old Effect) {
		t.Helper()
		write(other, func(tx *SQLiteTx) error {
			requireStoreOK(t, tx.LaterEffect(old, time.Unix(0, 0), time.Hour))
			requireStoreOK(t, tx.FailEffect(old, time.Unix(0, 0), time.Second, time.Hour, "过期"))
			return tx.DeleteEffect(old)
		})
	}
	stale(first)
	if got := head(t, db); !reflect.DeepEqual(got, merged) {
		t.Fatalf("旧代次改动了队首 %+v", got)
	}
	write(db, func(tx *SQLiteTx) error { return tx.DeleteEffect(merged) })
	second := head(t, other)
	if second.Key != "two" || second.Seq <= first.Seq {
		t.Fatalf("第二行 %+v", second)
	}
	write(other, func(tx *SQLiteTx) error { requireStoreOK(t, tx.DeleteEffect(second)); return enqueue(tx) })
	reused := head(t, other)
	if reused.Seq <= second.Seq || reused.Generation != first.Generation {
		t.Fatalf("重新入队 %+v", reused)
	}
	stale(first)
	if got := head(t, db); !reflect.DeepEqual(got, reused) {
		t.Fatalf("旧序号改动了新行 %+v", got)
	}
}

func TestEffectRetry(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, filepath.Join(t.TempDir(), "retry.db"))
	requireStoreOK(t, db.Write(ctx, func(tx *SQLiteTx) error {
		requireStoreOK(t, tx.Enqueue("delivery", "w", json.RawMessage("1")))
		return tx.Enqueue("delivery", "next", json.RawMessage("2"))
	}))
	e := head(t, db)
	now, err := time.Parse(milliseconds, e.Due)
	requireStoreOK(t, err)
	now = now.Add(time.Hour)
	const base = 150 * time.Millisecond
	const ceiling = 500 * time.Millisecond
	message := strings.Repeat("界", 1001)
	for i, want := range []time.Duration{base, 2 * base, ceiling, ceiling} {
		requireStoreOK(t, db.Write(ctx, func(tx *SQLiteTx) error { return tx.FailEffect(e, now, base, ceiling, message) }))
		got := head(t, db)
		if got.Attempts != i+1 || got.Due != now.Add(want).Format(milliseconds) || got.Error != strings.Repeat("界", 1000) {
			t.Fatalf("第 %d 次退避：%+v", i+1, got)
		}
	}
	requireStoreOK(t, db.Write(ctx, func(tx *SQLiteTx) error { return tx.LaterEffect(e, now, base) }))
	got := head(t, db)
	if got.Attempts != 4 || got.Due != now.Add(base).Format(milliseconds) || got.Error != strings.Repeat("界", 1000) {
		t.Fatalf("推迟后 %+v", got)
	}
	requireStoreOK(t, db.View(ctx, func(r SQLiteReader) error {
		rows, err := r.DueEffects("delivery", now, 2)
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].Key != "next" {
			t.Fatalf("已到期行 %+v", rows)
		}
		rows, err = r.DueEffects("delivery", now.Add(base), 1)
		if err != nil {
			return err
		}
		if len(rows) != 1 {
			t.Fatalf("读取上限未生效 %+v", rows)
		}
		return nil
	}))
	requireStoreOK(t, db.Write(ctx, func(tx *SQLiteTx) error { return tx.Enqueue("delivery", "w", json.RawMessage("true")) }))
	if got := head(t, db); got.Attempts != 0 || got.Error != "" || got.Due >= now.Format(milliseconds) {
		t.Fatalf("新代次继承了退避 %+v", got)
	}
}

func TestEffectRejectedPayload(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "invalid.db"))
	requireStoreOK(t, db.Write(t.Context(), func(tx *SQLiteTx) error {
		// 第一项在 Go 校验时失败，第二项在递增计数器后才被库拒绝。
		for _, raw := range []string{"NaN", strings.Repeat("[", 1001) + "0" + strings.Repeat("]", 1001)} {
			if err := tx.Enqueue("delivery", "bad", json.RawMessage(raw)); err == nil {
				t.Fatal("接受了无效负载")
			}
		}
		return tx.Enqueue("delivery", "valid", json.RawMessage("true"))
	}))
	if got := head(t, db); got.Key != "valid" || got.Seq != 1 {
		t.Fatalf("拒绝后队列仍受影响 %+v", got)
	}
}
