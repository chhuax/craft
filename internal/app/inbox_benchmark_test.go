package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

const benchInboxCount = 1000

// BenchmarkInboxReceive 每轮提交一千个不同 ID；并发调用者各有一条独立写连接。
func BenchmarkInboxReceive(b *testing.B) {
	for _, workers := range []int{1, 8} {
		b.Run(fmt.Sprintf("Writers_%d", workers), func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "receive.db")
			sources := make([]*SourceContext, workers)
			wake := make(chan struct{}, 1)
			for i := range sources {
				sources[i] = testSource(b, inboxDB(b, path), func() {
					select {
					case wake <- struct{}{}:
					default:
					}
				})
			}
			for round := 0; b.Loop(); round++ {
				var group sync.WaitGroup
				for worker, source := range sources {
					group.Go(func() {
						for n := worker; n < benchInboxCount; n += workers {
							got, err := source.Receive(b.Context(), inboxTrigger(strconv.Itoa(round*benchInboxCount+n)))
							if err != nil || got != "queued" {
								b.Errorf("入队结果 %q, %v", got, err)
								return
							}
						}
					})
				}
				group.Wait()
			}
			b.ReportMetric(float64(b.N*benchInboxCount)/b.Elapsed().Seconds(), "events/s")
		})
	}
}

// BenchmarkInboxDrain 比较同一文件布局、事件及引擎处理路径，只计量消费这一千行的耗时。
func BenchmarkInboxDrain(b *testing.B) {
	for _, threads := range []int{100, 10000} {
		for _, mode := range []string{"Row", "Batch"} {
			b.Run(fmt.Sprintf("Threads_%d/%s", threads, mode), func(b *testing.B) {
				for b.Loop() {
					b.StopTimer()
					db := inboxDB(b, filepath.Join(b.TempDir(), "drain.db"))
					r := testRuntime(b, db)
					seedDrain(b, r, threads, benchInboxCount)
					b.StartTimer()
					for range benchInboxCount / batchLimit {
						drain := (*Runtime).drain
						if mode == "Batch" {
							drain = drainBatch
						}
						_, err := drain(r, b.Context())
						checkErr(b, err)
					}
					b.StopTimer()
					checkErr(b, r.repo.Read(b.Context(), func(read engine.Reader) error {
						events, err := read.EventsSince(0, benchInboxCount+1)
						if len(events) != benchInboxCount {
							b.Fatalf("实际消费 %d 条", len(events))
						}
						return err
					}))
					checkErr(b, db.Close())
					b.StartTimer()
				}
				b.ReportMetric(float64(b.N*benchInboxCount)/b.Elapsed().Seconds(), "events/s")
			})
		}
	}
}

func seedDrain(b *testing.B, r *Runtime, threads, events int) {
	b.Helper()
	ctx := b.Context()
	checkErr(b, r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
		u := r.repo.in(ctx, tx)
		for i := range threads {
			ref := benchRef(i)
			if err := u.PutThread(engine.Thread{Ref: ref, Key: engine.ThreadKey(ref), CreatedAt: benchAt}); err != nil {
				return err
			}
		}
		// 两种库都命中相同的 100 个既有对象，每个对象有十条事件。
		for i := range events {
			tr := engine.Trigger{ID: strconv.Itoa(i), Source: "feed", Event: "change", At: benchAt, Threads: []engine.ThreadRef{benchRef(i % 100)}}
			raw, err := json.Marshal(tr)
			if err != nil {
				return err
			}
			if _, err := tx.InsertInbox(tr.ID, raw, tr.At); err != nil {
				return err
			}
		}
		return nil
	}))
}

// 保留完整的保存点方案作同机对照，包含队尾检查、坏行隔离和提交回调。
func drainBatch(r *Runtime, ctx context.Context) (bool, error) {
	var pending bool
	err := r.repo.db.View(ctx, func(read store.SQLiteReader) error {
		var err error
		pending, err = read.HasInbox()
		return err
	})
	if err != nil || !pending {
		return false, err
	}
	var more bool
	err = r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
		rows, err := tx.InboxBatch(batchLimit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			err := tx.Savepoint(ctx, func(child *store.SQLiteTx) error { return r.consume(ctx, child, row) })
			if err == nil {
				continue
			}
			if !errors.Is(err, errPoison) {
				return fmt.Errorf("消费 inbox[%q]: %w", row.ID, err)
			}
			// 保存点已撤销该行的全部引擎写入和广播。删除坏行以免每拍都阻塞后续事件。
			if delErr := tx.DeleteInbox(row.ID); delErr != nil {
				return errors.Join(err, delErr)
			}
			tx.AfterCommit(func() { slog.Error("已移除损坏的 inbox 行", "id", row.ID, "seq", row.Seq, "error", err) })
		}
		// 不足一批已读到队尾；后来入队的行有自己的提交唤醒或外部版本通知。
		if len(rows) == batchLimit {
			more, err = tx.HasInbox()
		}
		return err
	})
	return err == nil && more, err
}

// BenchmarkIdlePass 包含定时拍的外部写入检查与完整空闲 Pass，不计等待时长。
func BenchmarkIdlePass(b *testing.B) {
	for _, threads := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d", threads), func(b *testing.B) {
			db := inboxDB(b, filepath.Join(b.TempDir(), "idle.db"))
			r := testRuntime(b, db)
			seedDrain(b, r, threads, 0)
			for b.Loop() {
				if _, err := db.DataVersion(b.Context()); err != nil {
					b.Fatal(err)
				}
				r.runPass(b.Context())
			}
		})
	}
}
