package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

type benchmarkExtension struct{}

func (benchmarkExtension) Execute(_ context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	if p.Preview {
		return outbound.Reply{Target: "one", Summary: "update one"}, nil
	}
	return outbound.Reply{Value: json.RawMessage(`{"id":"one"}`)}, nil
}

func (benchmarkExtension) Verify(context.Context, protocol.ActionVerifyParams) (outbound.Verification, json.RawMessage, error) {
	return "", nil, errors.New("基准不应核实正在发送的动作")
}

func seedOutbox(b *testing.B, db *store.SQLite, count int, now time.Time) {
	b.Helper()
	docs := make([]store.Doc, count)
	for i := range docs {
		id := strconv.Itoa(i)
		at := now.Add(-time.Duration(count-i) * time.Hour)
		r := outbound.Result{ID: id, State: outbound.Done, Level: outbound.Auto, Source: outbound.Session, Action: "a", At: at, AutoAt: new(at.UnixMilli()), Params: json.RawMessage(`{}`), Receipt: json.RawMessage(`{"id":"one"}`)}
		raw, err := json.Marshal(r)
		benchCheck(b, err)
		docs[i] = store.Doc{Key: id, Value: raw}
	}
	benchCheck(b, db.Atomic(b.Context(), func(tx store.Tx) error { return tx.PutMany("outbox", docs) }))
}

// BenchmarkSQLiteAct 保持背景记录数恒定；夹具写入和清理均在计时外。
// yes 从真正的待确认记录开始计时，包含重新预览、发送和回执持久化。
func BenchmarkSQLiteAct(b *testing.B) {
	for _, count := range []int{100, 10000} {
		for _, mode := range []string{"auto", "confirm-preview", "confirm-yes"} {
			b.Run(fmt.Sprintf("%s/%d", mode, count), func(b *testing.B) {
				ctx := b.Context()
				db := outboxDB(b)
				now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
				seedOutbox(b, db, count, now)
				s, err := outbound.New(outbound.Config{Outbox: NewSQLiteOutbox(db), Executor: benchmarkExtension{}, Clock: func() time.Time { return now }, Rate: outbound.RateLimit{Max: 5, Window: time.Minute},
					Actions: []outbound.Action{{ID: "a", Level: outbound.Auto, Reversible: true}, {ID: "c", Level: outbound.Confirm}}})
				benchCheck(b, err)
				q := outbound.Request{Source: outbound.Session, IdempotencyKey: "current", Action: "c", Params: json.RawMessage(`{"body":"public text"}`)}
				if mode == "auto" {
					q.Action = "a"
				}
				var want error
				if mode == "confirm-preview" {
					want = outbound.NeedsConfirmation
				}
				b.ReportAllocs()
				for b.Loop() {
					b.StopTimer()
					benchCheck(b, db.Atomic(ctx, func(tx store.Tx) error { return tx.Delete("outbox", q.IdempotencyKey) }))
					if mode == "confirm-yes" {
						q.Yes = false
						if _, err := s.Act(ctx, q); !errors.Is(err, outbound.NeedsConfirmation) {
							b.Fatalf("准备待确认记录: %v", err)
						}
						q.Yes = true
					}
					b.StartTimer()
					if _, err := s.Act(ctx, q); !errors.Is(err, want) {
						b.Fatalf("执行 %s: %v", mode, err)
					}
				}
			})
		}
	}
}
