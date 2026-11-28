package app

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/outbound"
)

func TestOutboundConfig(t *testing.T) {
	for _, tc := range []struct {
		config  string
		dir, ok bool
	}{
		{`{"organization":{"write":"auto"}}`, false, true},
		{`{"organization":{"write":"invalid"}}`, false, false},
		{`{"organizatoin":{}}`, false, false}, {`null`, false, false}, {`{"organization":null}`, false, false},
		{`{"organization":{"write":"deny","write":"auto"}}`, false, false}, {"", true, false},
	} {
		t.Run(tc.config, func(t *testing.T) {
			home := t.TempDir()
			var log bytes.Buffer
			path := filepath.Join(home, "outbound.json")
			if tc.dir {
				checkErr(t, os.Mkdir(path, 0700))
			} else {
				checkErr(t, os.WriteFile(path, []byte(tc.config), 0600))
			}
			d := NewDaemon(DaemonConfig{StateDir: home, Clock: newTestClock(), Logger: slog.New(slog.NewTextHandler(&log, nil))})
			err := d.Start(t.Context())
			if err == nil {
				checkErr(t, d.Stop(context.Background()))
			}
			if (err == nil) != tc.ok || err != nil && !strings.Contains(err.Error(), "outbound.json") {
				t.Fatal(err)
			}
			if tc.ok && (!strings.Contains(log.String(), "WARN") || !strings.Contains(log.String(), "write")) {
				t.Fatal(log.String())
			}
		})
	}
}

func TestMergeAttention(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	for _, tc := range []struct {
		name    string
		base    []engine.AttentionItem
		pending []outbound.Confirmation
		kinds   []string
	}{
		{"空", nil, nil, nil},
		{"只含确认", nil, []outbound.Confirmation{{ID: "x", At: at}}, []string{"confirm"}},
		{"混合顺序", []engine.AttentionItem{{ID: "a", Kind: "answer", Severity: "action", At: at.Add(time.Second).Format(utcMillis)}, {ID: "b", Kind: "unavailable", Severity: "warning", At: at.Format(utcMillis)}}, []outbound.Confirmation{{ID: "x", At: at}, {ID: "y", At: at}}, []string{"answer", "confirm", "confirm", "unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := mergeAttention(tc.base, tc.pending)
			var kinds []string
			for n, item := range items {
				kinds = append(kinds, item.Kind)
				if n > 0 && items[n-1].At == item.At && items[n-1].Severity == item.Severity && items[n-1].ID >= item.ID {
					t.Fatal(items)
				}
				if item.Kind == "confirm" && (item.Ref.Attention == "" || !slices.Equal(item.Actions, []string{"confirm", "withdraw"})) {
					t.Fatal(item)
				}
			}
			if !slices.Equal(kinds, tc.kinds) {
				t.Fatal(kinds)
			}
		})
	}
}

func BenchmarkMergedAttention(b *testing.B) {
	var set engine.AttentionSet
	var pending []outbound.Confirmation
	for i := range 1000 {
		id := fmt.Sprint(i)
		at := time.Unix(int64(i), 0).UTC()
		set.Works = append(set.Works, engine.Work{ID: id, UpdatedAt: at.Format(utcMillis), Attention: &engine.Question{ID: id}})
		pending = append(pending, outbound.Confirmation{ID: id, At: at})
	}
	b.ReportAllocs()
	for b.Loop() {
		if n := len(mergeAttention(engine.Attention(set), pending)); n != 2000 {
			b.Fatal(n)
		}
	}
}
