package method

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoFile(t *testing.T, dir, name, body string) {
	t.Helper()
	file := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func repoGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}

func testRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	// 隔离调用进程继承的仓库、配置和对象路径，所有写操作只落入临时仓库。
	for _, pair := range os.Environ() {
		env, _, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(strings.ToUpper(env), "GIT_") {
			t.Setenv(env, "")
			if err := os.Unsetenv(env); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_COUNT", "0")
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00Z")
	t.Setenv("GIT_CEILING_DIRECTORIES", tmp)
	dir := filepath.Join(tmp, "repo")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	repoGit(t, dir, "init", "-q", "--template=", "-b", "main")
	repoGit(t, dir, "config", "--local", "user.name", "测试")
	repoGit(t, dir, "config", "--local", "user.email", "test@example.org")
	repoFile(t, dir, "base", "起点\n")
	repoFile(t, dir, ".gitignore", "out/\n")
	repoGit(t, dir, "add", "--", ".")
	repoGit(t, dir, "commit", "-qm", "起点\n\nTask: change/old")
	repoGit(t, dir, "switch", "-qc", "work")
	return dir
}

func TestGitFacts(t *testing.T) {
	dir := testRepo(t)
	ctx := t.Context()
	base, err := BaseCommit(ctx, dir, "main")
	if err != nil || base != repoGit(t, dir, "rev-parse", "HEAD") {
		t.Fatal(base, err)
	}
	if s, err := WorktreeStatus(ctx, dir); err != nil || len(s) != 0 {
		t.Fatal(s, err)
	}
	repoFile(t, dir, "base", "甲\n乙\n")
	repoFile(t, dir, "- 空 格", "\x00\x01")
	repoGit(t, dir, "add", "--", "- 空 格")
	repoFile(t, dir, "out/ignored", "x")
	s, err := WorktreeStatus(ctx, dir)
	if err != nil || len(s) != 2 {
		t.Fatal(s, err)
	}
	got := map[string]string{}
	for _, p := range s {
		got[p.Path] = p.XY
	}
	if got["base"] != ".M" || got["- 空 格"] != "A." {
		t.Fatal(got)
	}
	stat, err := DiffStat(ctx, dir, base)
	if err != nil || stat.Files != 2 || stat.Added != 2 || stat.Deleted != 1 {
		t.Fatal(stat, err)
	}
	for _, bad := range []string{"", "--help", "main..HEAD", "no-such"} {
		if _, err := BaseCommit(ctx, dir, bad); err == nil {
			t.Error(bad)
		}
	}
	for _, call := range []func(context.Context, string, string) error{
		func(c context.Context, d, s string) error { _, e := Commits(c, d, s); return e },
		func(c context.Context, d, s string) error { _, e := DiffStat(c, d, s); return e },
	} {
		if err := call(ctx, dir, "--help"); err == nil {
			t.Fatal("选项被解释为提交")
		}
	}
	repoGit(t, dir, "commit", "-qam", "正文")
	repoGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	b, err := BaseCommit(ctx, dir, "main")
	if err != nil || b == base {
		t.Fatal(b, err)
	}
}

func TestGitErrors(t *testing.T) {
	dir := testRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := WorktreeStatus(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	expired, stop := context.WithTimeout(t.Context(), 0)
	defer stop()
	if _, err := WorktreeStatus(expired, dir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	plain := filepath.Join(filepath.Dir(dir), "plain")
	if err := os.Mkdir(plain, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := WorktreeStatus(t.Context(), plain); err == nil {
		t.Fatal("非仓库")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := WorktreeStatus(t.Context(), dir); !errors.Is(err, exec.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestGitOutputLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	w := gitOutput{limit: 4, cancel: cancel}
	if _, err := w.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("e")); err == nil || !w.overflow || ctx.Err() == nil || w.data.Len() > 4 {
		t.Fatal(w.data.Len(), err)
	}
}

func TestGitRecordBoundaries(t *testing.T) {
	for _, raw := range []string{"8\t1\tfile", "8\tq\tfile\x00", "8\t1\t\x00"} {
		if _, err := parseDiffStat(raw); err == nil {
			t.Error(raw)
		}
	}
	if s, err := parseDiffStat("-\t-\t- space\n\tname\x00"); err != nil || s != (DiffStats{Files: 1}) {
		t.Fatal(s, err)
	}
	for _, raw := range []string{"bad\x00x\x00", strings.Repeat("a", 40) + "\x00truncated"} {
		if _, err := parseCommits(raw); err == nil {
			t.Fatal(raw)
		}
	}
}

func BenchmarkCommitList(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		var input strings.Builder
		for i := range n {
			fmt.Fprintf(&input, "%040x\x00标题\n\nTask: change/t-%d\n\x00", i, i)
		}
		data := input.String()
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				commits, err := parseCommits(data)
				if err != nil {
					b.Fatal(err)
				}
				p := Continuation("change", nil, commits)
				if len(p.Committed) != n {
					b.Fatal(len(p.Committed))
				}
			}
		})
	}
}

func BenchmarkDiffStat(b *testing.B) {
	for _, n := range []int{10000, 100000} {
		var input strings.Builder
		for i := range n {
			fmt.Fprintf(&input, "8\t3\tdir/%d\x00", i)
		}
		data := input.String()
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			for b.Loop() {
				s, err := parseDiffStat(data)
				if err != nil || s.Files != n {
					b.Fatal(s, err)
				}
			}
		})
	}
}
