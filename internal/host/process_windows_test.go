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
	// 作业对象的终止是异步的：TerminateJobObject 发出后，进程对象要过几毫秒才变成已触发状态，
	// 所以不能用超时 0 立刻问，否则会偶发 WAIT_TIMEOUT（258）。这里仍然要求它确实被终止，
	// 只是给一个有上限的等待。
	if state, err := syscall.WaitForSingleObject(h, 5000); err != nil || state != syscall.WAIT_OBJECT_0 {
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
