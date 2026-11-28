package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestMemoryEnqueue(t *testing.T) {
	s := NewMemory().(*memoryStore)
	ctx := t.Context()
	rollback := errors.New("撤销")
	for _, abort := range []bool{true, false} {
		err := s.Atomic(ctx, func(u Unit) error {
			v := json.RawMessage("123")
			if err := u.Effects().Enqueue("delivery", "w", v); err != nil {
				return err
			}
			v[0] = '9'
			err := u.Atomic(ctx, func(child Unit) error {
				if err := child.Effects().Enqueue("delivery", "w", v); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if err := u.Effects().Enqueue("delivery", "w", json.RawMessage("NaN")); err == nil {
				t.Error("接受了非法 JSON")
			}
			if err := u.Effects().Enqueue("delivery", "w", json.RawMessage("456")); err != nil {
				return err
			}
			if abort {
				return rollback
			}
			return nil
		})
		if abort {
			if !errors.Is(err, rollback) || len(s.committed.Load().effects) != 0 {
				t.Fatalf("回滚后仍保留入队调用：%v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	got := s.committed.Load().effects
	if len(got) != 2 {
		t.Fatalf("入队次数 %d", len(got))
	}
	for i, v := range []string{"123", "456"} {
		e := got[i]
		if e.Kind != "delivery" || e.Key != "w" || !bytes.Equal(e.Value, []byte(v)) {
			t.Errorf("第 %d 次入队 = %+v", i, e)
		}
	}
}

func TestMemoryCanceledBeforeCommit(t *testing.T) {
	s := NewMemory()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	err := s.Atomic(ctx, func(u Unit) error {
		u.AfterCommit(func() { called = true })
		if err := u.Emit(Event{Type: "work.created", At: at1}); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || called {
		t.Fatalf("取消时仍提交：%v，回调 %v", err, called)
	}
	err = s.Read(t.Context(), func(r Reader) error {
		rows, err := r.EventsSince(0, 1)
		if len(rows) != 0 {
			t.Errorf("取消后事件仍可见 %v", rows)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
