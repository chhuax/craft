//go:build unix

package exthost

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestUnixGroupStopsDescendants(t *testing.T) {
	e, o := fixture(t, "tree")
	o.ShutdownTimeout = 100 * time.Millisecond
	h := launch(t, e, o)
	b, err := os.ReadFile(filepath.Join(e.Dir, "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid == syscall.Getpgrp() {
		t.Fatal("孙进程没有独立进程组", pgid, err)
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	// 孙进程持有 stderr，Stop 能排空日志即表明其已退出；再等系统回收孤儿。
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("孙进程 %d 未回收: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
