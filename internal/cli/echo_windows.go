package cli

import (
	"errors"
	"os"
	"syscall"
)

var setConsoleMode = syscall.NewLazyDLL("kernel32.dll").NewProc("SetConsoleMode")

func disableEcho(input *os.File) (func() error, error) {
	h := syscall.Handle(input.Fd())
	var mode uint32
	err := syscall.GetConsoleMode(h, &mode)
	// 管道和重定向文件都没有控制台模式，可以直接读取。
	const invalidHandle = syscall.Errno(6)
	if errors.Is(err, invalidHandle) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	set := func(m uint32) error {
		if ok, _, err := setConsoleMode.Call(uintptr(h), uintptr(m)); ok == 0 {
			return err
		}
		return nil
	}
	const echoInput = 0x0004
	if err := set(mode &^ echoInput); err != nil {
		return nil, err
	}
	return func() error { return set(mode) }, nil
}
