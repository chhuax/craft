package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chhuax/craft/internal/alarm"
)

// Policy 尚无保留期字段，使用 06-runtime §7 的默认时间窗口。
const (
	workRetention    = 90 * 24 * time.Hour
	triggerRetention = 30 * 24 * time.Hour
	eventRetention   = 30 * 24 * time.Hour
)

func (d *Daemon) prune(ctx context.Context, _ alarm.RunRequest) error {
	now := d.cfg.Clock.Now().UTC()
	result, err := d.runtime.pass.Prune(ctx, now.Add(-triggerRetention).Format(utcMillis), now.Add(-workRetention).Format(utcMillis), now.Add(-eventRetention).Format(utcMillis))
	if err != nil {
		return err
	}
	// Pass 引擎在最外层写事务提交成功后才返回 ID；目录故障只影响当前目录。
	for _, id := range result.WorkIDs {
		if err := removeScratch(d.cfg.StateDir, id); err != nil {
			d.cfg.Logger.Warn("清理 Work 的 scratch 失败", "work", id, "error", err)
		}
	}
	return nil
}

func removeScratch(stateDir, id string) (err error) {
	if !validScratchID(id) {
		return nil
	}
	// 准备器可能从未建过目录，也可能恰好被外部清除；两者均已达到清理目的。
	defer func() {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
	}()
	state, err := filepath.EvalSymlinks(stateDir)
	if err != nil {
		return fmt.Errorf("解析状态目录: %w", err)
	}
	base := filepath.Join(state, "scratch")
	info, err := os.Lstat(base)
	if err != nil {
		return fmt.Errorf("检查 scratch 根目录: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("scratch 根路径 %q 不是独立目录", base)
	}
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("解析 scratch 根目录: %w", err)
	}
	if real != base {
		return fmt.Errorf("scratch 根目录已重定向到 %q", real)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return fmt.Errorf("打开 scratch 根目录: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	current, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("核对 scratch 根目录: %w", err)
	}
	if !os.SameFile(info, current) {
		return fmt.Errorf("scratch 根目录已被替换")
	}
	return removeScratchEntry(root, base, id)
}

func removeScratchEntry(root *os.Root, base, id string) error {
	info, err := root.Lstat(id)
	if err != nil {
		return fmt.Errorf("检查 scratch 子目录: %w", err)
	}
	// 最后一层是链接时只移除目录项，悬挂链接也不需要查找目标。
	if info.Mode()&os.ModeSymlink != 0 {
		return root.Remove(id)
	}
	if !info.IsDir() {
		return fmt.Errorf("scratch/%s 不是目录", id)
	}
	path := filepath.Join(base, id)
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("解析 scratch 子目录: %w", err)
	}
	if real != path {
		return fmt.Errorf("scratch/%s 已重定向到 %q", id, real)
	}
	// 单分量入口和固定的目录句柄约束实际删除；即使校验后换成链接也不会递归跟随。
	return root.RemoveAll(id)
}
