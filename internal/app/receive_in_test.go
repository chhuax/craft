package app

import (
	"errors"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func TestReceiveInRollback(t *testing.T) {
	ctx := t.Context()
	s := sqliteRepository(t)
	p, err := engine.NewPolicy(engine.Settings{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, err := engine.New(engine.Config{Store: s, Scope: benchScope{}, Clock: func() string { return benchAt }, Policy: func() engine.Policy { return p }})
	if err != nil {
		t.Fatal(err)
	}
	tr := engine.Trigger{ID: "one", At: benchAt, Threads: []engine.ThreadRef{benchRef(0)}}
	failure := errors.New("撤销外层事务")
	err = s.Atomic(ctx, func(u engine.Unit) error {
		result, err := e.ReceiveIn(u, tr)
		if err != nil || result != engine.Stored {
			t.Fatalf("事务内接收 %s, %v", result, err)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
	result, err := e.Receive(ctx, tr)
	if err != nil || result != engine.Stored {
		t.Fatalf("回滚后重放 %s, %v", result, err)
	}
}
