package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistExtensionSelection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	dir := filepath.Join(home, "extensions", "sample")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"id":"sample","version":"1","protocol":2,"exec":["fixture"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	daemonCommand(t, "daemon", "start")
	t.Cleanup(func() { command(t, home, 0, "daemon", "stop") })
	command(t, home, 6, "alarm", "run", "sample", "poll")
	command(t, home, 5, "alarm", "run", "absent", "poll")
	daemonCommand(t, "ext", "disable", "sample")
	daemonCommand(t, "daemon", "stop")
	daemonCommand(t, "daemon", "start")
	list := daemonCommand(t, "ext", "list")
	state := list["extensions"].(map[string]any)["sample"].(map[string]any)
	if state["status"] != "disabled" {
		t.Fatal(state)
	}
	command(t, home, 5, "ext", "enable", "absent")
	daemonCommand(t, "ext", "enable", "sample")
	daemonCommand(t, "daemon", "stop")
	daemonCommand(t, "daemon", "start")
	list = daemonCommand(t, "ext", "list")
	state = list["extensions"].(map[string]any)["sample"].(map[string]any)
	if state["status"] != "incompatible" {
		t.Fatal(state)
	}
}

func TestOfflineExtensions(t *testing.T) {
	home := t.TempDir()
	extra := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", extra+string(os.PathListSeparator)+t.TempDir())
	ext := filepath.Join(extra, "invalid")
	if err := os.MkdirAll(ext, 0700); err != nil {
		t.Fatal(err)
	}
	list := daemonCommand(t, "ext", "list")
	if list["running"] != false || len(list["rejected"].(map[string]any)) != 1 {
		t.Fatal(list)
	}
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sample.log"), []byte("one\ntwo\nthree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got := daemonCommand(t, "ext", "logs", "sample", "--tail", "2")["lines"].([]any)
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatal(got)
	}
	command(t, home, 2, "ext", "logs", "../escape")
	c := command(t, home, 6, "alarm", "list")
	if !strings.Contains(c.Error.Message, "daemon") {
		t.Fatal(c)
	}
}
