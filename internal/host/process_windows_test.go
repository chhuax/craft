//go:build windows

package host

import (
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestJob(t *testing.T) {
	p, pid := tree(t, t.Context())
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if state, err := syscall.WaitForSingleObject(h, 0); err != nil || state != syscall.WAIT_OBJECT_0 {
		t.Fatal(state, err)
	}
}

func TestNativeCommandLength(t *testing.T) {
	cmd := &exec.Cmd{Path: "a.exe", Args: []string{"a.exe", strings.Repeat("🧪", 16380)}}
	if err := checkCommand(cmd); err != nil {
		t.Fatal(err)
	}
	cmd.Args[1] += "🧪"
	if err := checkCommand(cmd); err == nil {
		t.Fatal("超过 UTF-16 长度上限")
	}
	cmd.Path = "a.cmd"
	if err := checkCommand(cmd); err == nil {
		t.Fatal("接受了命令解释器脚本")
	}
}
