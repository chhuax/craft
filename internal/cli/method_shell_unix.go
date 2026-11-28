//go:build !windows

package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func methodShell(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "sh", "-c", script)
	// 管道中的验证进程也要随超时终止，否则返回后仍可能改动刚取过快照的文件。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return cmd
}
