package store_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/store"
)

func TestMemoryStore(t *testing.T) {
	runStoreTests(t, func(t *testing.T, logger *slog.Logger) store.Store {
		return store.NewMemory(logger)
	})
}

func TestSQLiteStore(t *testing.T) {
	runStoreTests(t, func(t *testing.T, logger *slog.Logger) store.Store {
		t.Helper()
		s, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "store.db"), logger)
		requireOK(t, err)
		t.Cleanup(func() { requireOK(t, s.Close()) })
		return s
	})
}

const kv = "watch_kv"

func requireOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func checkDoc(t *testing.T, tx store.ReadTx, collection, key, want string) {
	t.Helper()
	got, found, err := tx.Get(collection, key)
	requireOK(t, err)
	// 空字节串不是合法 JSON，可用它表示期望该键缺失。
	if found != (want != "") || string(got) != want {
		t.Fatalf("%s[%q] = %s, %v，期望 %q", collection, key, got, found, want)
	}
}

type storeFactory func(*testing.T, *slog.Logger) store.Store

func runStoreTests(t *testing.T, factory storeFactory) {
	for name, run := range map[string]func(*testing.T, storeFactory){
		"Documents":         testDocuments,
		"Rollback":          testRollback,
		"AfterCommit":       testAfterCommit,
		"ReadIsolation":     testReadIsolation,
		"Active":            testActive,
		"Rejected":          testRejected,
		"ReadAt":            testReadAt,
		"ConcurrentWriters": testConcurrentWriters,
	} {
		t.Run(name, func(t *testing.T) { run(t, factory) })
	}
}

func testDocuments(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	const original = `{ "z":9007199254740993, "a":{"unknown":[true,null]} }`
	requireOK(t, s.Atomic(t.Context(), func(tx store.Tx) error {
		for _, collection := range []string{"watch_trigger", "watch_disposition", "watch_thread", "watch_work",
			"watch_decision", "watch_trigger_thread", "watch_event", kv} {
			value := json.RawMessage(original)
			requireOK(t, tx.Put(collection, "a", value))
			value[0] = '['
			checkDoc(t, tx, collection, "a", original)
		}
		requireOK(t, tx.Put(kv, "b", json.RawMessage("false")))
		return tx.Put(kv, "b", json.RawMessage("true"))
	}))
	requireOK(t, s.Read(t.Context(), func(tx store.ReadTx) error {
		all, err := tx.List(kv, store.All)
		requireOK(t, err)
		if len(all) != 2 || string(all["a"]) != original || string(all["b"]) != "true" {
			t.Fatalf("全量文档 = %s", all)
		}
		all["a"][0] = '['
		raw, _, err := tx.Get(kv, "a")
		requireOK(t, err)
		raw[0] = '['
		checkDoc(t, tx, kv, "a", original)
		return nil
	}))
	requireOK(t, s.Atomic(t.Context(), func(tx store.Tx) error {
		requireOK(t, tx.Delete(kv, "a"))
		return tx.Delete(kv, "a")
	}))
	requireOK(t, s.Read(t.Context(), func(tx store.ReadTx) error {
		checkDoc(t, tx, kv, "a", "")
		checkDoc(t, tx, "watch_trigger", "a", original)
		checkDoc(t, tx, kv, "b", "true")
		return nil
	}))
}

func testRollback(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	ctx := t.Context()
	fail := errors.New("放弃修改")
	var hooks []string
	write := func(tx store.Tx, key string) error {
		tx.AfterCommit(func() { hooks = append(hooks, key) })
		return tx.Put(kv, key, json.RawMessage("1"))
	}
	outer := func(tx store.Tx) error {
		requireOK(t, write(tx, "before"))
		err := tx.Atomic(ctx, func(inner store.Tx) error {
			requireOK(t, inner.Delete(kv, "before"))
			requireOK(t, write(inner, "dropped"))
			requireOK(t, inner.Atomic(ctx, func(deep store.Tx) error { return write(deep, "deep") }))
			return fail
		})
		if !errors.Is(err, fail) {
			t.Fatalf("内层回滚 = %v", err)
		}
		checkDoc(t, tx, kv, "before", "1")
		checkDoc(t, tx, kv, "dropped", "")
		checkDoc(t, tx, kv, "deep", "")
		requireOK(t, tx.Atomic(ctx, func(inner store.Tx) error { return write(inner, "inner") }))
		return write(tx, "after")
	}
	err := s.Atomic(ctx, func(tx store.Tx) error {
		requireOK(t, outer(tx))
		return fail
	})
	if !errors.Is(err, fail) || len(hooks) != 0 {
		t.Fatalf("外层回滚 = %v，回调 = %v", err, hooks)
	}
	requireOK(t, s.Read(ctx, func(tx store.ReadTx) error {
		docs, err := tx.List(kv, store.All)
		if len(docs) != 0 {
			t.Fatalf("回滚后仍有 %s", docs)
		}
		return err
	}))
	requireOK(t, s.Atomic(ctx, outer))
	if !slices.Equal(hooks, []string{"before", "inner", "after"}) {
		t.Fatalf("回调顺序 = %v", hooks)
	}
	requireOK(t, s.Read(ctx, func(tx store.ReadTx) error {
		for _, key := range hooks {
			checkDoc(t, tx, kv, key, "1")
		}
		checkDoc(t, tx, kv, "dropped", "")
		checkDoc(t, tx, kv, "deep", "")
		return nil
	}))
}

func testAfterCommit(t *testing.T, factory storeFactory) {
	var log bytes.Buffer
	s := factory(t, slog.New(slog.NewTextHandler(&log, nil)))
	ctx := t.Context()
	var hooks []int
	requireOK(t, s.Atomic(ctx, func(tx store.Tx) error {
		tx.AfterCommit(func() {
			hooks = append(hooks, 1)
			panic("回调失败")
		})
		tx.AfterCommit(func() {
			hooks = append(hooks, 2)
			// 回调里另开写事务，验证外层已提交并且已经释放写锁。
			requireOK(t, s.Atomic(ctx, func(next store.Tx) error {
				checkDoc(t, next, kv, "committed", "true")
				return next.Put(kv, "observed", json.RawMessage("true"))
			}))
		})
		return tx.Put(kv, "committed", json.RawMessage("true"))
	}))
	if !slices.Equal(hooks, []int{1, 2}) || !strings.Contains(log.String(), "回调失败") {
		t.Fatalf("回调 = %v，日志 = %s", hooks, &log)
	}
	requireOK(t, s.Read(ctx, func(tx store.ReadTx) error {
		checkDoc(t, tx, kv, "observed", "true")
		return nil
	}))
}

func testReadIsolation(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	ctx := t.Context()
	requireOK(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.Put(kv, "n", json.RawMessage("1")) }))
	requireOK(t, s.Read(ctx, func(snapshot store.ReadTx) error {
		// 在读回调第一次取文档前提交另一笔写入，检查快照从回调开始即固定。
		requireOK(t, s.Atomic(ctx, func(write store.Tx) error {
			requireOK(t, write.Put(kv, "n", json.RawMessage("2")))
			return s.Read(ctx, func(fresh store.ReadTx) error {
				checkDoc(t, fresh, kv, "n", "1")
				return nil
			})
		}))
		checkDoc(t, snapshot, kv, "n", "1")
		return nil
	}))
	requireOK(t, s.Read(ctx, func(tx store.ReadTx) error {
		checkDoc(t, tx, kv, "n", "2")
		return nil
	}))
}

func testActive(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	for collection, field := range map[string]string{"watch_thread": "closed", "watch_work": "ended"} {
		requireOK(t, s.Atomic(t.Context(), func(tx store.Tx) error {
			for key, doc := range map[string]string{"open": `{"other":true}`, "done": fmt.Sprintf(`{"%s":{}}`, field), "null": fmt.Sprintf(`{"%s":null}`, field)} {
				requireOK(t, tx.Put(collection, key, json.RawMessage(doc)))
			}
			active, err := tx.List(collection, store.Active)
			requireOK(t, err)
			if len(active) != 1 || string(active["open"]) != `{"other":true}` {
				t.Fatalf("%s 活跃集 = %s", collection, active)
			}
			return nil
		}))
	}
}

func testRejected(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	requireOK(t, s.Atomic(t.Context(), func(tx store.Tx) error {
		requireOK(t, tx.Put(kv, "good", json.RawMessage("123")))
		for _, raw := range []string{"", "NaN", "Infinity", `{"x":}`, "{} []"} {
			if err := tx.Put(kv, "good", json.RawMessage(raw)); err == nil {
				t.Fatalf("接受了非法 JSON %q", raw)
			}
		}
		checkDoc(t, tx, kv, "good", "123")
		for _, collection := range []string{"watch_inbox", "watch_effect", "alarm_state", "ext_state", "unknown"} {
			if err := tx.Put(collection, "k", json.RawMessage("1")); err == nil {
				t.Fatalf("可写未注册集合 %s", collection)
			}
			if _, _, err := tx.Get(collection, "k"); err == nil {
				t.Fatalf("可读未注册集合 %s", collection)
			}
		}
		if _, err := tx.List(kv, store.Active); err == nil {
			t.Fatal("在未注册此索引的集合接受了 Active")
		}
		if _, err := tx.List("watch_thread", "unknown"); err == nil {
			t.Fatal("接受了未注册的谓词")
		}
		return nil
	}))
}

func testReadAt(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	const doc = `{"inputs":[{"kind":"trigger"},{"kind":"trigger","readAt":null},{"readAt":"2026-01-01T00:00:00.000Z","kind":"trigger"}]}`
	requireOK(t, s.Atomic(t.Context(), func(tx store.Tx) error { return tx.Put("watch_work", "w", json.RawMessage(doc)) }))
	requireOK(t, s.Read(t.Context(), func(tx store.ReadTx) error {
		got, found, err := tx.Get("watch_work", "w")
		requireOK(t, err)
		var work struct{ Inputs []map[string]json.RawMessage }
		requireOK(t, json.Unmarshal(got, &work))
		if !found || len(work.Inputs) != 3 {
			t.Fatalf("Work.inputs = %s", got)
		}
		_, legacy := work.Inputs[0]["readAt"]
		if legacy || string(work.Inputs[1]["readAt"]) != "null" || string(work.Inputs[2]["readAt"]) != `"2026-01-01T00:00:00.000Z"` {
			t.Fatalf("readAt 三态丢失：%s", got)
		}
		return nil
	}))
}

func testConcurrentWriters(t *testing.T, factory storeFactory) {
	s := factory(t, nil)
	ctx := t.Context()
	requireOK(t, s.Atomic(ctx, func(tx store.Tx) error { return tx.Put(kv, "n", json.RawMessage("0")) }))
	start := make(chan struct{})
	done := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			for range 20 {
				err := s.Atomic(ctx, func(tx store.Tx) error {
					raw, _, err := tx.Get(kv, "n")
					if err != nil {
						return err
					}
					n, err := strconv.Atoi(string(raw))
					if err != nil {
						return err
					}
					return tx.Put(kv, "n", json.RawMessage(strconv.Itoa(n+1)))
				})
				if err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	close(start)
	for range 2 {
		requireOK(t, <-done)
	}
	requireOK(t, s.Read(ctx, func(tx store.ReadTx) error {
		checkDoc(t, tx, kv, "n", "40")
		return nil
	}))
}
