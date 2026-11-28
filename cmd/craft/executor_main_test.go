package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

func TestExecutorCLI(t *testing.T) {
	if os.Getenv("CRAFT_TEST_CLI") == "1" {
		os.Args = []string{"craft", "watch", "report", "progress", "--summary", "已核对", "--json"}
		main()
		return
	}
	home := t.TempDir()
	db, err := store.OpenSQLite(t.Context(), filepath.Join(home, "craft.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	repo := app.NewSQLiteStore(db)
	err = repo.Atomic(t.Context(), func(u engine.Unit) error {
		for _, id := range []string{"first", "second"} {
			if err := u.PutWork(engine.Work{ID: id, Execution: &engine.Execution{Session: &engine.Session{Host: "codex", ID: "session-" + id}, Assignment: id}, Assignments: []engine.Assignment{{ID: id, State: engine.AssignmentRunning}}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		work, id, native, role, assignment string
		want                               int
	}{
		{"first", "session-first", "", "worker", "first", 0}, {"second", "session-first", "", "worker", "first", 5}, {"first", "other", "", "worker", "first", 5}, {"first", "session-first", "", "person", "first", 4}, {"first", "", "", "worker", "first", 4}, {"first", "", "session-first", "worker", "first", 0}, {"first", "session-first", "session-second", "worker", "first", 4}, {"first", "session-first", "", "worker", "old", 5},
	} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestExecutorCLI$")
		env := map[string]string{"CRAFT_TEST_CLI": "1", "CRAFT_HOME": home, "GORACE": "atexit_sleep_ms=0", host.EnvWork: tc.work, host.EnvHost: "codex", host.EnvRole: tc.role, host.EnvAssignment: tc.assignment, host.EnvSessionID: tc.id, host.EnvCodexThreadID: tc.native}
		for _, entry := range os.Environ() {
			k, _, _ := strings.Cut(entry, "=")
			if _, ok := env[k]; !ok {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			exit, ok := errors.AsType[*exec.ExitError](err)
			if !ok {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		var res struct{ OK bool }
		if code != tc.want || json.Unmarshal(out, &res) != nil || res.OK != (tc.want == 0) {
			t.Fatalf("%+v: %d %s", tc, code, out)
		}
	}
	if err := repo.Read(t.Context(), func(r engine.Reader) error {
		w, err := r.GetWork("second")
		if err == nil && w.Assignments[0].Report != nil {
			t.Fatal("错误归属产生报告")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
