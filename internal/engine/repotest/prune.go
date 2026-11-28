package repotest

import (
	"errors"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func pruneRollback(t *testing.T, s engine.Store) {
	ctx := t.Context()
	const at = "2026-01-01T00:00:00.000Z"
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(engine.Work{ID: "w", Ended: &engine.WorkEnd{At: at}}) }))
	err := s.Atomic(ctx, func(u engine.Unit) error {
		got, err := u.Prune(at, "2026-01-02T00:00:00.000Z", at)
		must(t, err)
		if got.Works != 1 {
			t.Fatal(got)
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		t.Fatal(err)
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		w, err := r.GetWork("w")
		must(t, err)
		if w == nil {
			t.Fatal("回滚丢失 Work")
		}
		return nil
	}))
}
