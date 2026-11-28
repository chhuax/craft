package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/outbound/outboundtest"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

func outboxDB(t testing.TB) *store.SQLite {
	t.Helper()
	db, err := store.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "outbox.db"), nil)
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

// TestSQLiteOutbox 用独立文件库复用外发契约套件。
func TestSQLiteOutbox(t *testing.T) {
	outboundtest.Run(t, func(t *testing.T) outbound.Outbox { return NewSQLiteOutbox(outboxDB(t)) })
}

type receiptInterrupted struct{ outbound.Outbox }

func (b receiptInterrupted) Save(ctx context.Context, r *outbound.Result, limit outbound.RateLimit, clock func() time.Time) error {
	if r.State == outbound.Done {
		return io.ErrUnexpectedEOF
	}
	return b.Outbox.Save(ctx, r, limit, clock)
}

type recordingExtension struct{ sent, checked int }

func (e *recordingExtension) Execute(_ context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	if !p.Preview {
		e.sent++
	}
	return outbound.Reply{Summary: "one", Value: json.RawMessage(`{}`)}, nil
}

func (e *recordingExtension) Verify(context.Context, protocol.ActionVerifyParams) (outbound.Verification, json.RawMessage, error) {
	e.checked++
	return outbound.Applied, json.RawMessage(`{}`), nil
}

// TestRecoverAfterReopen 模拟扩展已生效而回执提交失败，检验关闭并重开文件后的恢复。
func TestRecoverAfterReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "interrupted.db")
	e := &recordingExtension{}
	cfg := outbound.Config{Executor: e, Clock: func() time.Time { return time.Unix(100, 0) }, Rate: outbound.RateLimit{Max: 2, Window: time.Minute}}
	q := outbound.Request{Action: "act", Params: json.RawMessage(`{}`), IdempotencyKey: "k", Yes: true, Source: outbound.Session}
	for _, stage := range []string{"回执提交中断", "重开文件恢复"} {
		t.Run(stage, func(t *testing.T) {
			db, err := store.OpenSQLite(ctx, path, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			cfg.Outbox = NewSQLiteOutbox(db)
			if stage == "回执提交中断" {
				cfg.Outbox = receiptInterrupted{cfg.Outbox}
			}
			s, err := outbound.New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "回执提交中断" {
				if _, err := s.Act(ctx, q); !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Fatal(err)
				}
				return
			}
			if err := s.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			r, err := s.Act(ctx, q)
			if err != nil || r.State != outbound.Done || string(r.Receipt) != "{}" || string(r.Preview.Value) != "{}" || e.sent != 1 || e.checked != 1 {
				t.Fatalf("重开后 %+v, %v；执行 %d、核实 %d", r, err, e.sent, e.checked)
			}
		})
	}
}
