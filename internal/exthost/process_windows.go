//go:build windows

package exthost

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
)

var (
	kernel32      = syscall.NewLazyDLL("kernel32.dll")
	createJob     = kernel32.NewProc("CreateJobObjectW")
	assignJob     = kernel32.NewProc("AssignProcessToJobObject")
	terminateJob  = kernel32.NewProc("TerminateJobObject")
	resumeProcess = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
)

func startTree(cmd *exec.Cmd) (func() error, error) {
	if filepath.Ext(cmd.Path) == "" {
		return nil, fmt.Errorf("exec[0] 在 Windows 上须明确指定文件后缀: %s", cmd.Path)
	}
	// 挂起创建、加入 Job 后才恢复，避免扩展在分配 Job 之前就派生子进程。
	if err := resumeProcess.Find(); err != nil {
		return nil, err
	}
	job, _, err := createJob.Call(0, 0)
	if job == 0 {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	closeJob := func() error { return syscall.CloseHandle(syscall.Handle(job)) }
	const createSuspended = 0x4
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createSuspended}
	if err := cmd.Start(); err != nil {
		return nil, errors.Join(err, closeJob())
	}
	err = attachAndResume(cmd, job)
	if err != nil {
		killErr := cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, errors.Join(err, killErr, closeJob())
	}
	return func() error {
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
