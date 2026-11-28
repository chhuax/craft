package method

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func reportDoc() Document {
	return Parse("requirements.md", scenarios+"## 假设与待确认\n- A-1：保留顺序\n")
}

func reviewer() VerifierReport {
	return VerifierReport{Version: 1, By: "复核人", Scenarios: []ScenarioVerdict{{"S-1", "pass", "TestRead"}, {"S-2", "pass", "TestOrder"}}}
}

func TestEvidenceChain(t *testing.T) {
	dir := testRepo(t)
	store, err := NewFileStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	snap := takeSnapshot(t, dir)
	another := strings.Repeat("f", 64)
	req := reportDoc()
	zero := 0
	ok := ClosingRun{Command: "go test ./...", Before: snap, After: snap, Exit: &zero}
	record := func(run ClosingRun) {
		t.Helper()
		if err := store.RecordClosing("one", instant, run); err != nil {
			t.Fatal(err)
		}
	}
	state := func(at string) EvidenceStatus {
		t.Helper()
		log, err := store.LoadLedger("one")
		if err != nil {
			t.Fatal(err)
		}
		return AssessEvidence(req, log, at)
	}
	review := func(r VerifierReport) error { return store.RecordReview("one", instant, snap, req, r) }
	if review(reviewer()) == nil || state(snap).Review != "none" {
		t.Fatal("无收尾记录")
	}
	record(ok)
	if s := state(snap); s.Closing != "pass" || s.Review != "none" {
		t.Fatal(s)
	}
	for _, mutate := range []func(*VerifierReport){
		func(r *VerifierReport) { r.Version = 2 }, func(r *VerifierReport) { r.By = " " }, func(r *VerifierReport) { r.Scenarios = nil },
		func(r *VerifierReport) { r.Scenarios = r.Scenarios[:1] }, func(r *VerifierReport) { r.Scenarios[1].ID = "S-9" },
		func(r *VerifierReport) { r.Scenarios[1].ID = "S-1" }, func(r *VerifierReport) { r.Scenarios[1].Evidence = "" },
		func(r *VerifierReport) { r.Scenarios[1].Result = "未知" }, func(r *VerifierReport) { r.Scenarios[1] = ScenarioVerdict{"S-2", "fail", " "} },
	} {
		r := reviewer()
		mutate(&r)
		if review(r) == nil {
			t.Error(r)
		}
	}
	for _, result := range []string{"fail", "unverified", "pass"} {
		r := reviewer()
		r.Scenarios[1].Result = result
		if err := review(r); err != nil {
			t.Fatal(err)
		}
		want := result
		if result == "unverified" {
			want = "fail"
		}
		if s := state(snap); s.Review != want {
			t.Error(s)
		}
	}
	if s := state(another); s.Closing != "stale" || s.Review != "stale" {
		t.Fatal(s)
	}
	if err := store.RecordReview("one", instant, another, req, reviewer()); err == nil {
		t.Fatal("过期的收尾验证")
	}
	log, err := store.LoadLedger("one")
	if err != nil || len(log) != 4 {
		t.Fatal(log, err)
	}
	last := log[len(log)-1]
	if last.Version != 1 || !last.At.Equal(instant) || len(last.Review.Pending) != 1 || last.Review.Pending[0].ID != "A-1" {
		t.Fatal(last)
	}
	changed := Parse("requirements.md", scenarios+"## 假设与待确认\n- A-2：保留其他数据\n")
	if s := AssessEvidence(changed, log, snap); s.Review != "stale" {
		t.Fatal(s)
	}
	if takeSnapshot(t, dir) != snap {
		t.Fatal("状态记录改变快照")
	}
	failed := 1
	for _, run := range []ClosingRun{{"检查", &failed, snap, snap}, {"检查", nil, snap, snap}, {"检查", &zero, another, snap}} {
		record(run)
		if s := state(snap); s.Closing == "pass" || s.Review == "pass" || review(reviewer()) == nil {
			t.Fatal(s)
		}
	}
}

func TestMethodState(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if tasks, err := store.LoadTasks("one"); err != nil || tasks != "" {
		t.Fatal(tasks, err)
	}
	if log, err := store.LoadLedger("one"); err != nil || len(log) > 0 {
		t.Fatal(log, err)
	}
	raw := card("read", "无", "a")
	if err := store.SaveTasks("one", raw); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadTasks("one")
	if err != nil || !strings.Contains(loaded, raw) || Parse("tasks.md", loaded).value("craft") != formatVersion {
		t.Fatal(loaded, err)
	}
	tasks, err := ParseTasks(loaded, nil)
	if err != nil || len(tasks) != 1 {
		t.Fatal(tasks, err)
	}
	future := "---\ncraft: 2\n---\n" + raw
	taskPath := filepath.Join(dir, ".craft", "method", "one.tasks.md")
	if err := os.WriteFile(taskPath, []byte(future), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTasks("one", raw); err == nil {
		t.Fatal("覆盖了未来版本清单")
	}
	if old, err := os.ReadFile(taskPath); err != nil || string(old) != future {
		t.Fatal(err, string(old))
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if err := store.RecordFact("one", instant.Add(time.Duration(i)*time.Second), "已复现"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	records, err := store.LoadLedger("one")
	if err != nil || len(records) != 8 {
		t.Fatal(records, err)
	}
	if err := store.RecordFact("one", instant, " "); err == nil {
		t.Fatal("空事实")
	}
	if err := store.RecordFact("../外部", instant, "事实"); err == nil {
		t.Fatal("越界")
	}
	for _, bad := range []string{"{\"version\":2}\n", "{"} {
		path := filepath.Join(dir, ".craft", "method", "one.jsonl")
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.LoadLedger("one"); err == nil {
			t.Fatal("损坏记录", bad)
		}
		if err := store.RecordFact("one", instant, "事实"); err == nil {
			t.Fatal("续写无效记录")
		}
		data, err := os.ReadFile(path)
		if err != nil || !reflect.DeepEqual(data, []byte(bad)) {
			t.Fatal(string(data), err)
		}
	}
}
