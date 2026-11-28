//go:build unix

package exthost

import (
	"errors"
	"os/exec"
	"syscall"
)

func startTree(cmd *exec.Cmd) (func() error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid
	return func() error {
		// 组长可能先退出，负的组号仍能找到继承标准流的后代。
		err := syscall.Kill(-pgid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}, nil
}
