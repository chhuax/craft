//go:build unix

package host

import (
	"syscall"
	"testing"
)

func TestProcessGroup(t *testing.T) {
	p, pid := tree(t, t.Context())
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid == syscall.Getpgrp() || pgid == pid {
		t.Fatal(pgid, err)
	}
	// 孙进程同时持有 stderr；Close 只有在它被回收并关闭管道后才能返回。
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}
