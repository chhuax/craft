//go:build windows

package exthost

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestWindowsJobStopsDescendants(t *testing.T) {
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
	handle, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := syscall.CloseHandle(handle); err != nil {
			t.Error(err)
		}
	}()
	if state, err := syscall.WaitForSingleObject(handle, 0); err != nil || state != syscall.WAIT_TIMEOUT {
		t.Fatalf("孙进程未运行：%v %v", state, err)
	}
	if err := h.Stop(); err != nil {
		t.Fatal(err)
	}
	if state, err := syscall.WaitForSingleObject(handle, 1000); err != nil || state != syscall.WAIT_OBJECT_0 {
		t.Fatalf("孙进程仍在运行：%v %v", state, err)
	}
}

func TestWindowsExecSuffix(t *testing.T) {
	e, o := fixture(t, "normal")
	e.Manifest.Exec[0] = "extension"
	// 即使同名无后缀文件存在，也不能改为执行旁边的 .exe。
	if err := os.WriteFile(filepath.Join(e.Dir, "extension"), []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if h, err := Start(t.Context(), e, o); err == nil {
		_ = h.Stop()
		t.Fatal("隐式补全了可执行后缀")
	}
}
