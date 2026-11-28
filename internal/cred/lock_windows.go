package cred

import (
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockFile(f *os.File) error {
	// 未置 FAIL_IMMEDIATELY 时，同步文件句柄会等到前一持有者关闭。
	const exclusive = 0x2
	var offset syscall.Overlapped
	if ok, _, err := lockFileEx.Call(f.Fd(), exclusive, 0, 1, 0, uintptr(unsafe.Pointer(&offset))); ok == 0 {
		return err
	}
	return nil
}
