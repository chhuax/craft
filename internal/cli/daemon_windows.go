//go:build windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Go 将控制台的 Ctrl+C 与 Ctrl+Break 都交给 os.Interrupt。
var daemonSignals = []os.Signal{os.Interrupt}

func detach(cmd *exec.Cmd) {
	const detachedProcess = 0x8
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | syscall.CREATE_NEW_PROCESS_GROUP}
}

func guardFile(path string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	// 共享模式为零，内核会在持有进程终止时释放独占句柄。
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if errors.Is(err, syscall.Errno(32)) {
		return nil, errOwned
	}
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// 句柄绑定到该次创建的进程，PID 被复用也不会误等或误杀。
func observeExit(pid int) (exitProbe, error) {
	h, err := syscall.OpenProcess(syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return exitProbe{}, err
	}
	return exitProbe{func() (bool, error) {
		state, err := syscall.WaitForSingleObject(h, 0)
		return state == syscall.WAIT_OBJECT_0, err
	}, func() error { return syscall.CloseHandle(h) }}, nil
}
