package app

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

func checkErr(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func inboxDB(t testing.TB, path string) *store.SQLite {
	t.Helper()
	db, err := store.OpenSQLite(t.Context(), path, nil)
	checkErr(t, err)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func feedManifest() exthost.Manifest {
	return exthost.Manifest{ID: "reader", WatchSources: []exthost.WatchSource{
		{ID: "feed", ThreadKinds: []exthost.ThreadKind{{ID: "item"}}, Events: []exthost.EventDecl{{Event: "change"}}},
		{ID: "side", ThreadKinds: []exthost.ThreadKind{{ID: "note"}}, Events: []exthost.EventDecl{{Event: "reply"}}},
	}}
}

func testSource(t testing.TB, db *store.SQLite, wake func()) *SourceContext {
	t.Helper()
	s, err := NewSourceContext(feedManifest(), db, wake)
	checkErr(t, err)
	return s
}

func inboxTrigger(id string) protocol.Trigger {
	return protocol.Trigger{ID: id, Source: "feed", Event: "change", At: "2026-01-02T08:00:00+08:00", Threads: []protocol.ThreadRef{
		{Source: "feed", Kind: "item", ID: id}, {Source: "side", Kind: "note", ID: "x"},
	}}
}

func TestSourceReceive(t *testing.T) {
	db := inboxDB(t, filepath.Join(t.TempDir(), "db"))
	var wakes int
	src := testSource(t, db, func() { wakes++ })
	for _, tc := range []struct {
		name string
		edit func(*protocol.Trigger)
	}{
		{"未声明来源", func(p *protocol.Trigger) { p.Source = "reader" }},
		{"其他来源事件", func(p *protocol.Trigger) { p.Event = "reply" }},
		{"未知对象来源", func(p *protocol.Trigger) { p.Threads[1].Source = "foreign" }},
		{"对象种类属于其他来源", func(p *protocol.Trigger) { p.Threads[1].Kind = "item" }},
		{"缺事件标识", func(p *protocol.Trigger) { p.ID = "" }},
		{"无对象", func(p *protocol.Trigger) { p.Threads = nil }},
		{"缺对象标识", func(p *protocol.Trigger) { p.Threads[0].ID = "" }},
		{"时间无效", func(p *protocol.Trigger) { p.At = "not a time" }},
		{"寻址无效", func(p *protocol.Trigger) { p.Addressed = new("all") }},
		{"非严格JSON", func(p *protocol.Trigger) { p.Meta = map[string]json.RawMessage{"n": []byte("NaN")} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := inboxTrigger("invalid")
			tc.edit(&p)
			result, err := src.Receive(t.Context(), p)
			rpc, ok := errors.AsType[*protocol.Error](err)
			if result != "" || !ok || rpc.Code != -32602 {
				t.Fatalf("拒绝结果 %q, %v", result, err)
			}
		})
	}
	if wakes != 0 {
		t.Fatal("拒绝后唤醒了 Pass")
	}
	checkErr(t, db.View(t.Context(), func(r store.SQLiteReader) error {
		rows, err := r.InboxBatch(10)
		if len(rows) != 0 {
			t.Fatal(rows)
		}
		return err
	}))
	for _, size := range []int{3999, 4000, 4001} {
		p := inboxTrigger(fmt.Sprint(size))
		p.Text = strings.Repeat("🍀", size)
		p.Actor = protocol.ActorRef{Source: "side", ID: "u", Name: new("人")}
		p.Human = true
		p.Addressed = new("mention")
		p.URL = new("example")
		p.Channel = &protocol.ChannelRef{Source: "side", Thread: []byte(`{"a":1}`), Extra: map[string]json.RawMessage{"big": []byte("99999999999999999999")}}
		p.Meta = map[string]json.RawMessage{"n": []byte("99999999999999999999")}
		for _, want := range []string{"queued", "duplicate"} {
			result, err := src.Receive(t.Context(), p)
			if result != want || err != nil {
				t.Fatalf("接收 %d = %q, %v", size, result, err)
			}
		}
		if _, ok := p.Meta["truncated"]; ok {
			t.Fatal("改写了调用方元数据")
		}
	}
	checkErr(t, db.View(t.Context(), func(r store.SQLiteReader) error {
		rows, err := r.InboxBatch(10)
		if err != nil {
			return err
		}
		if len(rows) != 3 || wakes != 6 {
			t.Fatalf("行数 %d, 唤醒 %d", len(rows), wakes)
		}
		for i, row := range rows {
			var got engine.Trigger
			if err := jsonv2.Unmarshal(row.Value, &got); err != nil {
				return err
			}
			if got.ID != row.ID || got.At != benchAt || row.At != got.At || got.Text != strings.Repeat("🍀", min(4000, 3999+i)) ||
				(string(got.Meta["truncated"]) == "true") != (i == 2) || !got.Human || got.Addressed != "mention" || got.URL != "example" ||
				got.Actor.Name != "人" || string(got.Channel.Extra["big"]) != "99999999999999999999" || string(got.Meta["n"]) != "99999999999999999999" ||
				engine.ThreadKey(got.Threads[1]) != "side:note:x" {
				t.Fatalf("规范化后的事件 %s", row.Value)
			}
		}
		return nil
	}))
}

func TestReceiveConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	const workers = 8
	var group sync.WaitGroup
	var queued atomic.Int64
	start := make(chan struct{})
	for range workers {
		src := testSource(t, inboxDB(t, path), func() {})
		group.Go(func() {
			<-start
			result, err := src.Receive(t.Context(), inboxTrigger("shared"))
			switch {
			case err != nil:
				t.Error(err)
			case result == "queued":
				queued.Add(1)
			case result != "duplicate":
				t.Errorf("结果 %q", result)
			}
		})
	}
	close(start)
	group.Wait()
	if queued.Load() != 1 {
		t.Fatalf("共入队 %d 次", queued.Load())
	}
}

func TestInboxCommitBeforeWake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db, peer := inboxDB(t, path), inboxDB(t, path)
	p := inboxTrigger("wake")
	calls := 0
	src := testSource(t, db, func() {
		calls++
		checkErr(t, peer.View(t.Context(), func(r store.SQLiteReader) error {
			rows, err := r.InboxBatch(1)
			if len(rows) != 1 || rows[0].ID != p.ID {
				t.Fatal("唤醒时行尚不可见")
			}
			return err
		}))
	})
	_, err := src.Receive(t.Context(), p)
	checkErr(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = src.Receive(ctx, inboxTrigger("failed"))
	if err == nil || calls != 1 {
		t.Fatalf("提交失败仍然唤醒 %d, %v", calls, err)
	}
}

// craft 是内核内置人事件的保留 source。即使有 manifest 声明了它（加载时本该被拒，这里绕过加载
// 直接构造，验证接收入口自己也守得住），经 watch/receive 发来的这类事件也必须被拒绝，
// 否则扩展能伪造「人发起的请求」。
func TestReceiveRejectsReservedSource(t *testing.T) {
	db := inboxDB(t, filepath.Join(t.TempDir(), "reserved.db"))
	m := exthost.Manifest{ID: "rogue", WatchSources: []exthost.WatchSource{
		{ID: exthost.ReservedSource, ThreadKinds: []exthost.ThreadKind{{ID: "item"}}, Events: []exthost.EventDecl{{Event: "person.request"}}},
	}}
	s, err := NewSourceContext(m, db, func() {})
	checkErr(t, err)
	_, err = s.Receive(t.Context(), protocol.Trigger{ID: "forged", Source: exthost.ReservedSource, Event: "person.request",
		At: "2026-01-02T08:00:00Z", Human: true, Threads: []protocol.ThreadRef{{Source: exthost.ReservedSource, Kind: "item", ID: "x"}}})
	if pe, ok := errors.AsType[*protocol.Error](err); !ok || pe.Code != -32602 {
		t.Fatalf("保留来源的事件应以协议错误拒绝，得到 %v", err)
	}
	checkErr(t, db.View(t.Context(), func(r store.SQLiteReader) error {
		if pending, err := r.HasInbox(); err != nil || pending {
			t.Fatalf("被拒绝的事件不能进入 inbox：pending=%v err=%v", pending, err)
		}
		return nil
	}))
}
