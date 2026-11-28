//go:build !windows

package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestDetachedUnixAndPrivateState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	t.Setenv("CRAFT_EXTENSIONS", "")
	daemonCommand(t, "daemon", "start")
	t.Cleanup(func() { command(t, home, 0, "daemon", "stop") })
	st, err := readDaemon(home)
	if err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(st.PID)
	if err != nil || group != st.PID {
		t.Fatal("daemon 未创建独立进程组", group, err)
	}
	info, err := os.Stat(filepath.Join(home, "daemon.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}

func TestForegroundSignals(t *testing.T) {
	for _, sig := range daemonSignals {
		t.Run(sig.String(), func(t *testing.T) {
			home := t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "daemon", "run", "--json")
			cmd.Env = append(os.Environ(), "CRAFT_HOME="+home, "CRAFT_EXTENSIONS=", "GORACE=atexit_sleep_ms=0")
			var out, diag bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &diag
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			for {
				if st, err := readDaemon(home); err == nil {
					if _, err := st.status(ctx); err == nil {
						break
					}
				}
				select {
				case err := <-done:
					t.Fatal(err, diag.String())
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-tick.C:
				}
			}
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil || !bytes.Contains(out.Bytes(), []byte(`"stopped":true`)) {
				t.Fatal(err, diag.String(), out.String())
			}
		})
	}
}
