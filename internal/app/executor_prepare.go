package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
)

type preparer struct {
	driver   host.Driver
	stateDir string
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

var unsafePart = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
var errTransient = errors.New("宿主或远端暂时不可用")

func defaultRef(ctx context.Context, repo string) (string, error) {
	if _, err := gitOutput(ctx, repo, "remote", "get-url", "origin"); err != nil {
		return "", fmt.Errorf("缺少 origin 远端: %w", err)
	}
	out, remoteErr := gitOutput(ctx, repo, "ls-remote", "--symref", "origin", "HEAD")
	branch := ""
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "ref: refs/heads/") && strings.HasSuffix(line, "\tHEAD") {
			branch = strings.TrimSuffix(strings.TrimPrefix(line, "ref: refs/heads/"), "\tHEAD")
			break
		}
	}
	if branch == "" {
		ref, _ := gitOutput(ctx, repo, "symbolic-ref", "-q", "refs/remotes/origin/HEAD")
		branch = strings.TrimPrefix(ref, "refs/remotes/origin/")
	}
	if branch == "" {
		for _, name := range []string{"main", "master"} {
			if _, err := gitOutput(ctx, repo, "rev-parse", "--verify", "refs/remotes/origin/"+name); err == nil {
				branch = name
				break
			}
		}
	}
	if branch == "" {
		if remoteErr != nil {
			return "", fmt.Errorf("查询远端默认分支: %w", errors.Join(errTransient, remoteErr))
		}
		return "", errors.New("缺少远端默认分支，请先核对 origin")
	}
	ref := "refs/remotes/origin/" + branch
	if _, err := gitOutput(ctx, repo, "fetch", "--quiet", "--refmap=", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return "", errors.Join(errTransient, err)
	}
	return ref, nil
}
func excludeWorktree(common, entry string) (err error) {
	file := filepath.Join(common, "info", "exclude")
	raw, err := os.ReadFile(file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.Contains("\n"+string(raw)+"\n", "\n"+entry+"\n") {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	_, err = fmt.Fprintln(f, "\n"+entry)
	return err
}

// scratch 可能已被执行方写入内容，只核对目录本身，保留其中的文件。
func privateDir(root *os.Root, name string) (err error) {
	if err := root.Mkdir(name, 0700); !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("路径 %q 已被占用，需要独立目录", name)
	}
	if runtime.GOOS == "windows" || info.Mode().Perm() == 0700 {
		return nil
	}
	// 对已打开的目录改权限，避免检查后路径被替换而改到其他文件。
	dir, err := root.OpenRoot(name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	return dir.Chmod(".", 0700)
}

func validScratchID(id string) bool {
	return filepath.IsLocal(id) && id != "." && !strings.ContainsAny(id, `/\`)
}

func (p preparer) scratch(workID string) (target engine.PreparedTarget, err error) {
	if !validScratchID(workID) {
		return target, fmt.Errorf("无效的 scratch 目录名 %q", workID)
	}
	root, err := os.OpenRoot(p.stateDir)
	if err != nil {
		return target, fmt.Errorf("打开状态目录: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	rel := filepath.Join("scratch", workID)
	for _, name := range []string{"scratch", rel} {
		if err := privateDir(root, name); err != nil {
			return target, fmt.Errorf("准备目录 %q: %w", name, err)
		}
	}
	return engine.PreparedTarget{Directory: filepath.Join(p.stateDir, rel)}, nil
}

// Prepare 按目标种类准备执行目录；git 目标须先经宿主规划副本路径。
func (p preparer) Prepare(ctx context.Context, t engine.TargetSnapshot, w engine.Work) (engine.PreparedTarget, error) {
	var empty engine.PreparedTarget
	if t.Kind == "scratch" {
		return p.scratch(w.ID)
	}
	if !filepath.IsAbs(t.Path) {
		return empty, errors.New("目标路径必须为绝对路径")
	}
	repo, err := filepath.EvalSymlinks(t.Path)
	if err != nil {
		return empty, err
	}
	info, err := os.Stat(repo)
	if err != nil {
		return empty, err
	}
	if !info.IsDir() {
		return empty, errors.New("目标不是目录")
	}
	if t.Kind == "dir" || t.Kind == "local" {
		return engine.PreparedTarget{Directory: repo}, nil
	}
	if t.Kind != "git" {
		return empty, fmt.Errorf("未知的目标类型 %q", t.Kind)
	}
	id := []rune(w.ID)
	id = id[:min(len(id), 12)]
	name := "watch-task-" + strings.Trim(unsafePart.ReplaceAllString(string(id), "-"), "-.")
	dest := p.driver.WorktreePath(repo, name)
	parent := filepath.Dir(dest)
	// 必须在 mkdir 或 git add 之前解析已有父目录，避免符号链接把副本导向人的目录。
	for probe := dest; ; probe = filepath.Dir(probe) {
		_, err := os.Lstat(probe)
		if errors.Is(err, os.ErrNotExist) && probe != filepath.Dir(probe) {
			continue
		}
		if err != nil {
			return empty, err
		}
		real, err := filepath.EvalSymlinks(probe)
		if err != nil {
			return empty, err
		}
		if filepath.Clean(probe) != real {
			return empty, fmt.Errorf("副本路径经过符号链接 %q", probe)
		}
		break
	}
	rel, err := filepath.Rel(repo, dest)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return empty, errors.New("副本必须位于目标仓库内")
	}
	common, err := gitOutput(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return empty, err
	}
	if _, err := os.Stat(dest); err == nil {
		top, e1 := gitOutput(ctx, dest, "rev-parse", "--show-toplevel")
		other, e2 := gitOutput(ctx, dest, "rev-parse", "--path-format=absolute", "--git-common-dir")
		dot, e3 := os.Lstat(filepath.Join(dest, ".git"))
		if e1 != nil || e2 != nil || e3 != nil || !dot.Mode().IsRegular() || filepath.Clean(top) != dest || filepath.Clean(other) != filepath.Clean(common) {
			return empty, fmt.Errorf("副本路径已被占用 %q", dest)
		}
		return engine.PreparedTarget{Directory: dest}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return empty, err
	}
	ref, err := defaultRef(ctx, repo)
	if err != nil {
		return empty, err
	}
	if err := excludeWorktree(common, "/"+filepath.ToSlash(filepath.Dir(rel))+"/"); err != nil {
		return empty, err
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return empty, err
	}
	if _, err := gitOutput(ctx, repo, "worktree", "add", "--detach", dest, ref); err != nil {
		entries, readErr := os.ReadDir(dest)
		if readErr == nil && len(entries) == 0 {
			err = errors.Join(err, os.Remove(dest))
		}
		return empty, err
	}
	return engine.PreparedTarget{Directory: dest}, nil
}
