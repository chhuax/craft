package repotest

import (
	"errors"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func deliveryWindow(t *testing.T, s engine.Store) {
	ctx := t.Context()
	w := engine.Work{ID: "w", Assignments: []engine.Assignment{{ID: "a", State: engine.AssignmentRunning}}, Undelivered: []string{"third", "second"}}
	for _, id := range []string{"first", "second", "third"} {
		w.Inputs = append(w.Inputs, engine.WorkInput{ID: id, Kind: "trigger", TriggerID: id})
	}
	check := func(r engine.Reader, after bool) {
		t.Helper()
		full, err := r.GetWork(w.ID)
		must(t, err)
		batch, err := r.GetDeliveryWindow(w.ID, []string{"third"})
		must(t, err)
		want := "third"
		if after {
			want = "second"
		}
		if !slices.Equal(batch.Head, []string{want}) || batch.Inputs["third"].ReadAt.Present != after || full.Inputs[2].ReadAt.Present != after || full.Undelivered[0] != want || full.Inputs[1].ReadAt.Present || full.Inputs[2].TriggerID != "third" {
			t.Fatalf("批次与完整聚合不一致 %+v %+v", batch, full)
		}
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutWork(w))
		absent, err := u.GetDeliveryWindow("missing", nil)
		must(t, err)
		if absent != nil {
			t.Fatal(absent)
		}
		change := func(c engine.Unit) error {
			batch, err := c.GetDeliveryWindow(w.ID, []string{"third"})
			must(t, err)
			in := batch.Inputs["third"]
			in.ReadAt = engine.ReadTime{Present: true}
			in.TriggerID = "与回执无关"
			batch.Inputs["third"] = in
			batch.Assignment.DeliveredAt = at
			batch.UpdatedAt = at
			must(t, c.PutDeliveryWindow(w.ID, *batch, 1))
			batch.Assignment.ID = "modified"
			batch.Head[0] = "modified"
			check(c, true)
			return nil
		}
		err = u.Atomic(ctx, func(c engine.Unit) error { must(t, change(c)); return errRollback })
		if !errors.Is(err, errRollback) {
			return err
		}
		check(u, false)
		return change(u)
	}))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		check(r, true)
		many, err := r.GetWorks(w.ID)
		must(t, err)
		open, err := r.OpenWorks()
		must(t, err)
		for _, v := range []*engine.Work{many[w.ID], &open[0]} {
			if v.Assignments[0].ID != "a" || v.UpdatedAt != at || v.Assignments[0].DeliveredAt != at || !v.Inputs[2].ReadAt.Present || !slices.Equal(v.Undelivered, []string{"second"}) {
				t.Fatalf("索引返回了旧工作 %+v", v)
			}
		}
		return nil
	}))
}

func queuedEffect(t *testing.T, s engine.Store) {
	ctx := t.Context()
	check := func(r engine.Reader, kind, key string, want bool) {
		got, err := r.HasEffect(kind, key)
		must(t, err)
		if got != want {
			t.Fatalf("%s/%s 排队=%v", kind, key, got)
		}
	}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		check(u, "delivery", "w", false)
		err := u.Atomic(ctx, func(c engine.Unit) error {
			must(t, c.Effects().Enqueue("delivery", "w", []byte("{}")))
			check(c, "delivery", "w", true)
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			return err
		}
		check(u, "delivery", "w", false)
		return u.Effects().Enqueue("delivery", "w", []byte("{}"))
	}))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		check(r, "delivery", "w", true)
		check(r, "stop", "w", false)
		check(r, "delivery", "other", false)
		return nil
	}))
}

func replyReceipts(t *testing.T, s engine.Store) {
	check := func(r engine.Reader, want engine.ReplyStatus) {
		t.Helper()
		got, err := r.GetReplyStatus("id")
		must(t, err)
		if got != want {
			t.Fatalf("回执 %q，期望 %q", got, want)
		}
	}
	ctx := t.Context()
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		check(u, "")
		for _, state := range []engine.ReplyStatus{engine.ReplyPending, engine.ReplyConfirm, engine.ReplySent, engine.ReplyDropped} {
			must(t, u.PutReplyStatus("id", state))
			check(u, state)
		}
		err := u.Atomic(ctx, func(c engine.Unit) error { must(t, c.PutReplyStatus("id", engine.ReplyPending)); return errRollback })
		if !errors.Is(err, errRollback) {
			return err
		}
		check(u, engine.ReplyDropped)
		return nil
	}))
	must(t, s.Read(ctx, func(r engine.Reader) error { check(r, engine.ReplyDropped); return nil }))
}
