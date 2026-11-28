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

// 入队时间必须来自注入的时钟：假时钟早于真实时间时，新入队的行也要在该时刻到期，
// 否则依赖假时钟的调度测试永远等不到「已到期」。
func TestEnqueueUsesInjectedClock(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2001, 2, 3, 4, 5, 6, 789_000_000, time.UTC)
	db, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "clock.db"), nil, WithClock(func() time.Time { return at }))
	requireStoreOK(t, err)
	t.Cleanup(func() { requireStoreOK(t, db.Close()) })
	requireStoreOK(t, db.Write(ctx, func(tx *SQLiteTx) error {
		return tx.Enqueue("delivery", "one", json.RawMessage("1"))
	}))
	got := head(t, db)
	if want := "2001-02-03T04:05:06.789Z"; got.Due != want || got.Since != want {
		t.Fatalf("due=%s since=%s，期望都是注入时钟的 %s", got.Due, got.Since, want)
	}
	requireStoreOK(t, db.View(ctx, func(r SQLiteReader) error {
		early, err := r.DueEffects("delivery", at.Add(-time.Millisecond), 10)
		requireStoreOK(t, err)
		onTime, err := r.DueEffects("delivery", at, 10)
		requireStoreOK(t, err)
		if len(early) != 0 || len(onTime) != 1 {
			t.Fatalf("到期判断：早 1ms 取到 %d 行，准点取到 %d 行", len(early), len(onTime))
		}
		return nil
	}))
}

func TestEffectStatsAndNextDue(t *testing.T) {
	ctx := t.Context()
	db := openTestDB(t, filepath.Join(t.TempDir(), "stats.db"))
	first := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	db.now = func() time.Time { return first }
	write := func(fn func(*SQLiteTx) error) { t.Helper(); requireStoreOK(t, db.Write(ctx, fn)) }
	enqueue := func(key string) {
		t.Helper()
		write(func(tx *SQLiteTx) error { return tx.Enqueue("delivery", key, json.RawMessage("1")) })
	}
	enqueue("a")
	a := head(t, db)
	next := first.Add(time.Hour)
	db.now = func() time.Time { return next }
	enqueue("b")
	var b Effect
	requireStoreOK(t, db.View(ctx, func(r SQLiteReader) error {
		rows, err := r.DueEffects("delivery", next, 2)
		if err != nil {
			return err
		}
		if len(rows) != 2 {
			t.Fatalf("到期行 %v", rows)
		}
		b = rows[1]
		due, err := r.NextEffectDue("delivery", first)
		if !due.Equal(next) {
			t.Errorf("下一到期时间 %v", due)
		}
		return err
	}))
	check := func(want EffectStats) {
		t.Helper()
		requireStoreOK(t, db.View(ctx, func(r SQLiteReader) error {
			got, err := r.EffectStats("delivery")
			if got != want {
				t.Errorf("统计 %+v，期望 %+v", got, want)
			}
			due, err2 := r.NextEffectDue("delivery", next.Add(time.Hour))
			if !due.IsZero() {
				t.Errorf("意外到期时间 %v", due)
			}
			return errors.Join(err, err2)
		}))
	}
	// 最近失败可以来自较老的行，也可能与前一次失败处于同一毫秒。
	write(func(tx *SQLiteTx) error {
		for range 3 {
			if err := tx.FailEffect(b, next, time.Second, time.Minute, "b"); err != nil {
				return err
			}
		}
		return tx.FailEffect(a, next, time.Second, time.Minute, "a")
	})
	check(EffectStats{Waiting: 2, Oldest: a.Since, MaxAttempts: 3, LastError: "a"})
	enqueue("a")
	check(EffectStats{Waiting: 2, Oldest: a.Since, MaxAttempts: 3, LastError: "b"})
	a = head(t, db)
	abort := errors.New("撤销")
	if err := db.Write(ctx, func(tx *SQLiteTx) error {
		if err := tx.DeleteEffect(a); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	check(EffectStats{Waiting: 2, Oldest: a.Since, MaxAttempts: 3, LastError: "b"})
	write(func(tx *SQLiteTx) error { return tx.DeleteEffect(a) })
	check(EffectStats{Waiting: 1, Oldest: b.Since, MaxAttempts: 3, LastError: "b"})
	write(func(tx *SQLiteTx) error { return tx.DeleteEffect(b) })
	check(EffectStats{})
}

func TestEffectSchedulingPlans(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "plans.db"))
	checkPlan(t, db, effectDue, "INDEX effect_due (kind=? AND due<?)")
	checkPlan(t, db, effectNextDue, "INDEX effect_due (kind=? AND due>?)")
	checkPlan(t, db, effectStatsQuery, "PRIMARY KEY (kind=?)", "INDEX effect_since", "INDEX effect_attempts", "INDEX effect_failure")
}
