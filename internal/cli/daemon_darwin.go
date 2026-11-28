package cli

import (
	"errors"
	"syscall"
)

// kqueue 登记在请求停止之前，NOTE_EXIT 绑定进程实例，稍后复用 PID 不会延长等待。
func observeExit(pid int) (exitProbe, error) {
	fd, err := syscall.Kqueue()
	if err != nil {
		return exitProbe{}, err
	}
	syscall.CloseOnExec(fd)
	change := syscall.Kevent_t{Ident: uint64(pid), Filter: syscall.EVFILT_PROC, Flags: syscall.EV_ADD | syscall.EV_ONESHOT, Fflags: syscall.NOTE_EXIT}
	if _, err := syscall.Kevent(fd, []syscall.Kevent_t{change}, nil, nil); err != nil {
		return exitProbe{}, errors.Join(err, syscall.Close(fd))
	}
	return exitProbe{func() (bool, error) {
		var events [1]syscall.Kevent_t
		n, err := syscall.Kevent(fd, nil, events[:], &syscall.Timespec{})
		if errors.Is(err, syscall.EINTR) {
			return false, nil
		}
		return n > 0, err
	}, func() error { return syscall.Close(fd) }}, nil
}
