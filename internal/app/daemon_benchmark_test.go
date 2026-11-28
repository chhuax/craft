package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
)

func BenchmarkDaemonReceive(b *testing.B) {
	for _, size := range []int{100, 10000} {
		b.Run(fmt.Sprintf("Threads_%d", size), func(b *testing.B) {
			for b.Loop() {
				b.StopTimer()
				measureDaemonBatch(b, size)
				b.StartTimer()
			}
			b.ReportMetric(float64(b.N*benchInboxCount)/b.Elapsed().Seconds(), "events/s")
		})
	}
}

func measureDaemonBatch(b *testing.B, size int) {
	b.Helper()
	d := NewDaemon(DaemonConfig{StateDir: b.TempDir()})
	db, err := store.OpenSQLite(b.Context(), filepath.Join(d.cfg.StateDir, "craft.db"), nil)
	checkErr(b, err)
	d.db = db
	m := feedManifest()
	s := &m.WatchSources[0]
	s.Events[0].Default = "auto"
	s.Events[0].Strength = "explicit"
	checkErr(b, d.startRuntime(b.Context(), []exthost.Extension{{Manifest: m}}))
	defer func() { checkErr(b, d.shutdown()) }()
	seedDrain(b, d.runtime, size, 0)
	h, err := newExtensionHandler(m, d.runtime, d.health)
	checkErr(b, err)
	r1, w1 := io.Pipe()
	r2, w2 := io.Pipe()
	kernel := exthost.NewConn(r1, w2, h, 0)
	ext := exthost.NewConn(r2, w1, nil, 0)
	defer func() { checkErr(b, ext.Close()); checkErr(b, kernel.Close()) }()
	var count atomic.Int32
	decided := make(chan struct{})
	d.runtime.repo.Subscribe(func(e engine.Event) {
		if e.Type == "decision.recorded" && count.Add(1) == benchInboxCount {
			close(decided)
		}
	})
	b.StartTimer()
	for i := range benchInboxCount {
		event := inboxTrigger("incoming-" + strconv.Itoa(i))
		event.Threads = event.Threads[:1]
		var result string
		checkErr(b, ext.Call(b.Context(), "watch/receive", event, &result))
		if result != "queued" {
			b.Fatal(result)
		}
	}
	select {
	case <-decided:
	case <-b.Context().Done():
		b.Fatal(b.Context().Err())
	}
	b.StopTimer()
	checkErr(b, db.View(b.Context(), func(r store.SQLiteReader) error {
		stats, err := r.EffectStats("launch")
		if stats.Waiting != benchInboxCount {
			b.Fatalf("launch 行数 %d", stats.Waiting)
		}
		return err
	}))
}

func BenchmarkDaemonShortTransaction(b *testing.B) {
	for _, method := range []string{"thread/describe", "thread/closed"} {
		b.Run(method, func(b *testing.B) {
			r := testRuntime(b, inboxDB(b, filepath.Join(b.TempDir(), "db")))
			h, err := newExtensionHandler(feedManifest(), r, newDaemonHealth(nil))
			checkErr(b, err)
			ref := engine.NewThreadRef("feed", "item", "target")
			payloads := make([]json.RawMessage, 2)
			for i := range payloads {
				p := map[string]any{"ref": ref, "label": strconv.Itoa(i)}
				if method == "thread/closed" {
					p = map[string]any{"thread": ref, "at": benchAt, "outcome": "done", "detail": strconv.Itoa(i)}
				}
				payloads[i], err = json.Marshal(p)
				checkErr(b, err)
			}
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				_, err := h(context.Background(), method, payloads[i%2])
				checkErr(b, err)
			}
		})
	}
}
