package method

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Snapshot 绑定 HEAD 与 Git 可见的未提交内容。仅从索引读标志，只打开改动文件；
// 不刷新索引、不写对象库。无法证明隐藏文件、子模块或冲突状态时返回错误。
func Snapshot(ctx context.Context, repo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	top, err := runGit(ctx, repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSuffix(top, "\n")
	if runtime.GOOS == "windows" {
		dir = strings.TrimSuffix(dir, "\r")
	}
	head, err := commitID(ctx, dir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("快照需要可读取的 HEAD：%w", err)
	}
	if err := checkVisibleIndex(ctx, dir); err != nil {
		return "", err
	}
	paths, err := WorktreeStatus(ctx, dir)
	if err != nil {
		return "", err
	}
	digest, err := hashChanges(ctx, dir, head, paths)
	if err != nil {
		return "", err
	}
	end, err := commitID(ctx, dir, "HEAD")
	if err != nil {
		return "", err
	}
	if head != end {
		return "", fmt.Errorf("计算快照期间 HEAD 已改变")
	}
	return digest, nil
}

func hashChanges(ctx context.Context, dir, head string, paths []PathStatus) (_ string, err error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", fmt.Errorf("打开快照根目录：%w", err)
	}
	defer closeResource(root, &err)
	// Git 对暂存文件与未跟踪文件分组输出，排序后暂存同一内容不会改变摘要。
	slices.SortStableFunc(paths, func(a, b PathStatus) int { return strings.Compare(a.Path, b.Path) })
	sum := sha256.New()
	// 标准库的 hash.Hash.Write 保证返回 nil 错误。
	sum.Write([]byte(strconv.Itoa(formatVersion) + "\x00" + head + "\x00"))
	buf := make([]byte, 64<<10)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if strings.HasPrefix(p.submodule, "S") || strings.Contains(p.XY, "U") || p.XY == "AA" || p.XY == "DD" || strings.HasSuffix(p.Path, "/") {
			return "", fmt.Errorf("无法为冲突文件或嵌套仓库 %q 计算快照", p.Path)
		}
		digest := "deleted"
		// Git 已认定删除的路径可能曾在一个目录下，而该目录现已换成符号链接。不可沿旧路径再读。
		if p.XY[1] != 'D' && p.XY != "D." {
			digest, err = workFileDigest(ctx, root, p.Path, buf)
			if err != nil {
				return "", fmt.Errorf("快照文件 %q：%w", p.Path, err)
			}
		}
		// 部分暂存时还要绑定索引版本；固定字段数与 NUL 分隔可容纳文件名中的换行和制表符。
		index := ""
		if p.XY != "??" && p.XY[0] != '.' && p.XY[1] != '.' {
			index = p.indexMode + ":" + p.indexOID
		}
		sum.Write([]byte(p.Path + "\x00" + digest + "\x00" + index + "\x00"))
	}
	return fmt.Sprintf("%x", sum.Sum(nil)), nil
}

func checkVisibleIndex(ctx context.Context, dir string) error {
	flags, err := runGit(ctx, dir, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	for entry := range strings.SplitSeq(flags, "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 3 || entry[1] != ' ' {
			return fmt.Errorf("索引标志格式无效")
		}
		if entry[0] == 'S' || entry[0] >= 'a' && entry[0] <= 'z' {
			return fmt.Errorf("索引隐藏了文件改动：%q", entry[2:])
		}
	}
	return nil
}

func workFileDigest(ctx context.Context, root *os.Root, rel string, buf []byte) (_ string, err error) {
	name := filepath.FromSlash(rel)
	info, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	mode := info.Mode()
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := root.Readlink(name)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("link:%x", sha256.Sum256([]byte(target))), nil
	case !mode.IsRegular():
		return "", fmt.Errorf("不支持的文件类型 %v", mode)
	}
	f, err := root.Open(name)
	if err != nil {
		return "", err
	}
	defer closeResource(f, &err)
	h := sha256.New()
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	end, err := root.Lstat(name)
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, end) || info.Size() != end.Size() || info.ModTime() != end.ModTime() || mode != end.Mode() {
		return "", fmt.Errorf("读取期间文件已改变")
	}
	return fmt.Sprintf("file:%t:%x", mode.Perm()&0111 != 0, h.Sum(nil)), nil
}
