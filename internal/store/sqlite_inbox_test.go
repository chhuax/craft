package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
)

func TestInboxQueue(t *testing.T) {
	s := openTestDB(t, filepath.Join(t.TempDir(), "inbox.db"))
	ctx := t.Context()
	err := s.Write(ctx, func(tx *SQLiteTx) error {
		for i, id := range []string{"z", "z", "a"} {
			added, err := tx.InsertInbox(id, json.RawMessage("1"), "now")
			if err != nil || added != (i != 1) {
				t.Fatalf("入队 %s = %v, %v", id, added, err)
			}
		}
		rows, err := tx.InboxBatch(1)
		if err != nil || len(rows) != 1 || rows[0].ID != "z" || rows[0].Seq != 1 || rows[0].At != "now" || string(rows[0].Value) != "1" {
			t.Fatalf("队首 %v, %v", rows, err)
		}
		failure := errors.New("撤销出队")
		err = tx.Savepoint(ctx, func(child *SQLiteTx) error {
			if err := child.DeleteInbox("z"); err != nil {
				return err
			}
			return failure
		})
		if !errors.Is(err, failure) {
			return err
		}
		rows, err = tx.InboxBatch(10)
		if err != nil || len(rows) != 2 || rows[0].ID != "z" || rows[1].ID != "a" || rows[1].Seq != 3 {
			t.Fatalf("保存点回滚后 %v, %v", rows, err)
		}
		for _, r := range rows {
			if err := tx.DeleteInbox(r.ID); err != nil {
				return err
			}
		}
		more, err := tx.HasInbox()
		if more {
			t.Fatal("删除后仍有待处理行")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
