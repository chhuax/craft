package cli

import (
	"context"
	"os/exec"
	"syscall"
)

func methodShell(ctx context.Context, script string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd.exe")
	// cmd.exe 自己解释 /c 后的引号。按普通 argv 转义会把带空格的路径和管道传错。
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `cmd.exe /d /s /c "` + script + `"`}
	return cmd
}
