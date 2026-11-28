package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestDetachedWindows(t *testing.T) {
	cmd := new(exec.Cmd)
	detach(cmd)
	if cmd.SysProcAttr.CreationFlags&(0x8|syscall.CREATE_NEW_PROCESS_GROUP) != (0x8 | syscall.CREATE_NEW_PROCESS_GROUP) {
		t.Fatal("后台进程未脱离控制台或没有独立进程组")
	}
	if len(daemonSignals) != 1 || daemonSignals[0] != os.Interrupt {
		t.Fatal("缺少控制台中断通知")
	}
}

// CI 的测试进程可能没有控制台。让测试二进制重入新控制台，然后在同一控制台里发送组中断。
func TestWindowsConsoleBreak(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if os.Getenv("CRAFT_TEST_CONSOLE") != "1" {
		helper := exec.CommandContext(ctx, exe, "-test.run=^TestWindowsConsoleBreak$")
		helper.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x10}
		helper.Env = append(os.Environ(), "CRAFT_TEST_CONSOLE=1", "CRAFT_HOME="+t.TempDir(), "CRAFT_EXTENSIONS=", "GORACE=atexit_sleep_ms=0")
		if out, err := helper.CombinedOutput(); err != nil {
			t.Fatal(err, string(out))
		}
		return
	}
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, exe, "daemon", "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Env = append(os.Environ(), "CRAFT_HOME="+home)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if s, err := readDaemon(home); err == nil {
			if _, err = s.status(ctx); err == nil {
				break
			}
		}
		select {
		case err := <-done:
			t.Fatal("就绪前退出", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
	send := syscall.NewLazyDLL("kernel32.dll").NewProc("GenerateConsoleCtrlEvent")
	if ok, _, err := send.Call(1, uintptr(cmd.Process.Pid)); ok == 0 {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "daemon.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("进程未完成清理", err)
	}
}
