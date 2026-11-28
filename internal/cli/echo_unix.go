//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

func disableEcho(input *os.File) (func() error, error) {
	fd := input.Fd()
	var old syscall.Termios
	err := terminalMode(fd, getTermios, &old)
	if errors.Is(err, syscall.ENOTTY) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	quiet := old
	quiet.Lflag &^= syscall.ECHO | syscall.ECHONL
	if err := terminalMode(fd, setTermios, &quiet); err != nil {
		return nil, err
	}
	return func() error { return terminalMode(fd, setTermios, &old) }, nil
}

func terminalMode(fd, request uintptr, mode *syscall.Termios) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(mode)))
	if errno != 0 {
		return errno
	}
	return nil
}
