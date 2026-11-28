package store

import (
	"path/filepath"
	"testing"
)

func TestPruneEventCursorAndEffectCompletion(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "prune.db"))
	const old = "2026-01-01T00:00:00.000Z"
	const cutoff = "2026-01-02T00:00:00.000Z"
	ctx := t.Context()
	requireStoreOK(t, s.Write(ctx, func(tx *SQLiteTx) error {
		if _, err := tx.tx.ExecContext(ctx, `WITH RECURSIVE seq(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM seq WHERE i<5001)
			INSERT INTO watch_event(key,value) SELECT CAST(i AS TEXT),json_object('rev',i,'at',?) FROM seq`, old); err != nil {
			return err
		}
		if err := tx.Put("watch_work", "w", []byte(`{"id":"w","ended":{"at":"2026-01-01T00:00:00.000Z"}}`)); err != nil {
			return err
		}
		if err := tx.Enqueue("reply", "message", []byte(`{"workId":"w"}`)); err != nil {
			return err
		}
		// outbound 由另一个库管理，里面的标识不形成 Watch 保护根。
		return tx.Put("outbox", "receipt", []byte(`{"workId":"w","state":"pending"}`))
	}))
	requireStoreOK(t, s.Write(ctx, func(tx *SQLiteTx) error {
		got, err := tx.Prune(cutoff, cutoff, cutoff)
		if err != nil {
			return err
		}
		if got.Events != 1 || got.Works != 0 {
			t.Fatal(got)
		}
		minRev, found, err := tx.Get("watch_kv", "event.min_rev")
		if err != nil || !found || string(minRev) != "2" {
			t.Fatalf("事件下界 %s, %v", minRev, err)
		}
		effect, err := tx.HeadEffect("reply")
		if err != nil {
			return err
		}
		if effect == nil {
			t.Fatal("清理消费了队列行")
		}
		return tx.DeleteEffect(*effect)
	}))
	requireStoreOK(t, s.Write(ctx, func(tx *SQLiteTx) error {
		got, err := tx.Prune(cutoff, cutoff, cutoff)
		if err != nil {
			return err
		}
		if got.Works != 1 || got.Events != 0 {
			t.Fatal(got)
		}
		if _, found, err := tx.Get("outbox", "receipt"); err != nil || !found {
			t.Fatalf("外发行 %t, %v", found, err)
		}
		return nil
	}))
}

func TestPrunePlans(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "plans.db"))
	for kind, idx := range map[string]string{"work": "prune_work_end", "decision": "prune_decision_at", "trigger": "prune_trigger_at", "thread": "prune_thread_close"} {
		t.Run(kind, func(t *testing.T) { checkPlan(t, s, pruneCandidateQuery(kind), idx, "prune_referenced") })
	}
}
