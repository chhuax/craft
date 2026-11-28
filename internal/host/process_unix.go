//go:build unix

package host

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

func startTree(cmd *exec.Cmd) (func(bool) error, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pgid := cmd.Process.Pid
	return func(terminate bool) error {
		if !terminate {
			return nil
		}
		// 组长可能先退出，负的组号仍能找到继承标准流的后代。
		// Darwin 在组长退出与 Wait 回收之间可能短暂返回 EPERM。仅在确认进程组
		// 已消失后把该错误当成功；持续无权发信号必须上报。
		deadline := time.Now().Add(250 * time.Millisecond)
		for {
			err := syscall.Kill(-pgid, syscall.SIGKILL)
			if err == nil || errors.Is(err, syscall.ESRCH) {
				return nil
			}
			if !errors.Is(err, syscall.EPERM) || !time.Now().Before(deadline) {
				return fmt.Errorf("终止宿主进程组 %d: %w", pgid, err)
			}
			time.Sleep(time.Millisecond)
		}
	}, nil
}

func checkCommand(*exec.Cmd) error { return nil }
