//go:build windows

package host

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf16"
)

var (
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	createJob     = kernel32.NewProc("CreateJobObjectW")
	assignJob     = kernel32.NewProc("AssignProcessToJobObject")
	terminateJob  = kernel32.NewProc("TerminateJobObject")
	resumeProcess = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

func startTree(cmd *exec.Cmd) (func(bool) error, error) {
	if filepath.Ext(cmd.Path) == "" {
		return nil, fmt.Errorf("exec[0] 在 Windows 上须明确指定文件后缀: %s", cmd.Path)
	}
	// 挂起创建、加入 Job 后才恢复，避免宿主在分配 Job 之前就派生子进程。
	if err := resumeProcess.Find(); err != nil {
		return nil, err
	}
	job, _, err := createJob.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	closeJob := func() error { return syscall.CloseHandle(syscall.Handle(job)) }
	const createSuspended = 0x4
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended | syscall.CREATE_NEW_PROCESS_GROUP | 0x08000000}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, closeJob())
	}
	err = attachAndResume(cmd, job)
	if err != nil {
		killErr := cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.Join(err, killErr, closeJob())
	}
	return func(terminate bool) error {
		if !terminate {
			return closeJob()
		}
		ok, _, callErr := terminateJob.Call(job, 1)
		if ok == 0 {
			return errors.Join(fmt.Errorf("TerminateJobObject: %w", callErr), closeJob())
		}
		return closeJob()
	}, nil
}

func attachAndResume(cmd *exec.Cmd, job uintptr) error {
	// os.Process 保有的句柄未承诺提供管理 Job、恢复进程所需的访问权。
	const setQuota, terminate, suspendResume = 0x0100, 0x0001, 0x0800
	h, err := syscall.OpenProcess(setQuota|terminate|suspendResume, false, uint32(cmd.Process.Pid))
	if err != nil {
		return err
	}
	defer func() { _ = syscall.CloseHandle(h) }()
	if ok, _, err := assignJob.Call(job, uintptr(h)); ok == 0 {
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if status, _, _ := resumeProcess.Call(uintptr(h)); status != 0 {
		return fmt.Errorf("NtResumeProcess: %#x", status)
	}
	return nil
}

func checkCommand(cmd *exec.Cmd) error {
	if !strings.EqualFold(filepath.Ext(cmd.Path), ".exe") {
		return fmt.Errorf("Windows 宿主命令须为原生 exe")
	}
	// CreateProcessW 的 32767 个 UTF-16 单元包括分隔空格、转义和末尾的 NUL。
	size := 0
	for _, arg := range cmd.Args {
		size += len(utf16.Encode([]rune(syscall.EscapeArg(arg)))) + 1
	}
	if size > 32767 {
		return fmt.Errorf("宿主命令超过 Windows 命令行上限")
	}
	return nil
}
