//go:build !windows

package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

var daemonSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}

func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func guardFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			err = errOwned
		}
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}
