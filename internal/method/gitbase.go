package method

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// git 的错误输出也要限长；超限立即取消子进程，不能把截断的记录当成完整事实。
type gitOutput struct {
	data     strings.Builder
	limit    int
	cancel   context.CancelFunc
	overflow bool
}

func (o *gitOutput) Write(p []byte) (int, error) {
	if len(p) > o.limit-o.data.Len() {
		o.overflow = true
		o.cancel()
		return 0, fmt.Errorf("git 输出超过 %d 字节", o.limit)
	}
	return o.data.Write(p)
}

func runGit(ctx context.Context, repo string, args ...string) (string, error) {
	if repo == "" {
		return "", fmt.Errorf("缺少 git 工作目录")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stdout := gitOutput{limit: 64 << 20, cancel: cancel}
	stderr := gitOutput{limit: 16 << 10, cancel: cancel}
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-pager", "--no-optional-locks", "--literal-pathspecs", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = repo
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	switch {
	case stdout.overflow || stderr.overflow:
		return "", fmt.Errorf("git %s 输出超过大小上限", args[0])
	case ctx.Err() != nil:
		return "", fmt.Errorf("git %s：%w", args[0], ctx.Err())
	case err != nil:
		return "", fmt.Errorf("在 %q 运行 git %s：%w %s", repo, args[0], err, strings.TrimSpace(stderr.data.String()))
	}
	return stdout.data.String(), nil
}

func gitOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c < '0' || (c > '9' && c < 'a') || c > 'f' {
			return false
		}
	}
	return true
}

func commitID(ctx context.Context, repo, ref string) (string, error) {
	out, err := runGit(ctx, repo, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	oid := strings.TrimSpace(out)
	if !gitOID(oid) {
		return "", fmt.Errorf("git 返回了无效的提交号 %q", oid)
	}
	return oid, nil
}

// BaseCommit 返回与 HEAD 的共同祖先。分支先查 origin 再查本地，也接受完整提交号。
func BaseCommit(ctx context.Context, repo, branch string) (string, error) {
	name := strings.TrimSpace(branch)
	if name == "" || strings.HasPrefix(name, "-") {
		return "", fmt.Errorf("基线名称无效：%q", branch)
	}
	refs := []string{"refs/remotes/origin/" + name, "refs/heads/" + name, name}
	if gitOID(name) {
		refs = []string{name}
	} else if _, err := runGit(ctx, repo, "check-ref-format", "refs/heads/"+name); err != nil {
		return "", fmt.Errorf("基线分支 %q：%w", name, err)
	}
	for _, ref := range refs {
		oid, err := commitID(ctx, repo, ref)
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				continue
			}
			return "", err
		}
		out, err := runGit(ctx, repo, "merge-base", "HEAD", oid)
		if err != nil {
			return "", fmt.Errorf("查找基线 %q 的共同祖先：%w", name, err)
		}
		base := strings.TrimSpace(out)
		if !gitOID(base) {
			return "", fmt.Errorf("基线 %q 的共同祖先无效", name)
		}
		return base, nil
	}
	return "", fmt.Errorf("找不到基线 %q", name)
}

// PathStatus 保留仓库相对路径和 Git 双列状态；部分暂存时还需索引内容来区分两份改动。
type PathStatus struct {
	Path, XY                       string
	indexOID, indexMode, submodule string
}

// WorktreeStatus 列出暂存、未暂存和未忽略的新文件。成功且列表为空即工作区干净。
func WorktreeStatus(ctx context.Context, repo string) ([]PathStatus, error) {
	raw, err := runGit(ctx, repo, "status", "--porcelain=v2", "-z", "--no-renames", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	var paths []PathStatus
	for raw != "" {
		line, rest, ok := strings.Cut(raw, "\x00")
		if !ok {
			return nil, fmt.Errorf("git status 的路径不完整")
		}
		raw = rest
		switch {
		case strings.HasPrefix(line, "? "):
			paths = append(paths, PathStatus{Path: line[2:], XY: "??"})
		case strings.HasPrefix(line, "1 "):
			f := strings.SplitN(line, " ", 9)
			if len(f) != 9 || len(f[1]) != 2 || f[8] == "" {
				return nil, fmt.Errorf("git status 的普通条目无效")
			}
			paths = append(paths, PathStatus{Path: f[8], XY: f[1], submodule: f[2], indexMode: f[4], indexOID: f[7]})
		case strings.HasPrefix(line, "u "):
			f := strings.SplitN(line, " ", 11)
			if len(f) != 11 || len(f[1]) != 2 || f[10] == "" {
				return nil, fmt.Errorf("git status 的冲突条目无效")
			}
			paths = append(paths, PathStatus{Path: f[10], XY: f[1], submodule: f[2]})
		default:
			return nil, fmt.Errorf("git status 返回了未知条目")
		}
	}
	return paths, nil
}

// Commit 保存完整提交说明，Task 行可以出现在末段之前。
type Commit struct{ ID, Message string }

// Commits 按从旧到新的拓扑顺序读取 base..HEAD；无效基线不能退回整个历史。
func Commits(ctx context.Context, repo, base string) ([]Commit, error) {
	if !gitOID(base) {
		return nil, fmt.Errorf("读取提交列表需要完整的基线提交号")
	}
	raw, err := runGit(ctx, repo, "log", "--no-show-signature", "--reverse", "--topo-order", "-z", "--format=%H%x00%B", base+"..HEAD", "--")
	if err != nil {
		return nil, err
	}
	return parseCommits(raw)
}

func parseCommits(raw string) ([]Commit, error) {
	var commits []Commit
	for raw != "" {
		oid, body, ok := strings.Cut(raw, "\x00")
		msg, tail, end := strings.Cut(body, "\x00")
		if !ok || !end || !gitOID(oid) {
			return nil, fmt.Errorf("提交列表格式无效")
		}
		commits = append(commits, Commit{oid, msg})
		raw = tail
	}
	return commits, nil
}

// DiffStats 统计已跟踪文件相对基线的净变化；二进制文件只计文件数。
type DiffStats struct{ Files, Added, Deleted int }

// DiffStat 统计基线提交与当前工作区的差异，改名按原路径和新路径分别统计。
func DiffStat(ctx context.Context, repo, base string) (DiffStats, error) {
	if !gitOID(base) {
		return DiffStats{}, fmt.Errorf("统计改动需要完整的基线提交号")
	}
	raw, err := runGit(ctx, repo, "diff", "--numstat", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", base, "--")
	if err != nil {
		return DiffStats{}, err
	}
	return parseDiffStat(raw)
}

func parseDiffStat(raw string) (DiffStats, error) {
	var s DiffStats
	for raw != "" {
		line, tail, ok := strings.Cut(raw, "\x00")
		a, rest, tab := strings.Cut(line, "\t")
		d, name, tab2 := strings.Cut(rest, "\t")
		if !ok || !tab || !tab2 || name == "" {
			return DiffStats{}, fmt.Errorf("git diff 统计条目不完整")
		}
		raw = tail
		s.Files++
		if a == "-" && d == "-" {
			continue
		}
		added, err := strconv.Atoi(a)
		if err != nil {
			return DiffStats{}, fmt.Errorf("解析增加行数：%w", err)
		}
		deleted, err := strconv.Atoi(d)
		if err != nil {
			return DiffStats{}, fmt.Errorf("解析删除行数：%w", err)
		}
		if added < 0 || deleted < 0 {
			return DiffStats{}, fmt.Errorf("git diff 返回负的行数")
		}
		s.Added += added
		s.Deleted += deleted
	}
	return s, nil
}
