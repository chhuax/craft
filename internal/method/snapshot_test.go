package method

import (
	"os"
	"path/filepath"
	"testing"
)

func takeSnapshot(t *testing.T, dir string) string {
	t.Helper()
	h, err := Snapshot(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 64 {
		t.Fatal(h)
	}
	return h
}

func TestSnapshotInputs(t *testing.T) {
	dir := testRepo(t)
	clean := takeSnapshot(t, dir)
	repoFile(t, dir, "out/generated", "不同")
	if takeSnapshot(t, dir) != clean {
		t.Fatal("被忽略文件进入快照")
	}
	repoFile(t, dir, "base", "改动\n")
	edit := takeSnapshot(t, dir)
	repoGit(t, dir, "add", "--", "base")
	if edit == clean || takeSnapshot(t, dir) != edit {
		t.Fatal("暂存同一内容")
	}
	repoFile(t, dir, "base", "工作区\n")
	partial := takeSnapshot(t, dir)
	repoFile(t, dir, "base", "另一暂存版本\n")
	repoGit(t, dir, "add", "--", "base")
	repoFile(t, dir, "base", "工作区\n")
	if takeSnapshot(t, dir) == partial {
		t.Fatal("丢失部分暂存的内容")
	}
	repoGit(t, dir, "add", "--", "base")
	staged := takeSnapshot(t, dir)
	repoGit(t, dir, "commit", "-qm", "改动")
	if takeSnapshot(t, dir) == staged {
		t.Fatal("提交后 HEAD 改变")
	}
	state := takeSnapshot(t, dir)
	for _, name := range []string{"- 空 格", "docs/interfaces/现状.md"} {
		repoFile(t, dir, name, "甲")
		a := takeSnapshot(t, dir)
		repoFile(t, dir, name, "乙")
		if state == a || takeSnapshot(t, dir) == a {
			t.Fatal(name)
		}
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(name))); err != nil {
			t.Fatal(err)
		}
		if takeSnapshot(t, dir) != state {
			t.Fatal("撤回", name)
		}
	}
	index := filepath.Join(dir, ".git", "index")
	for _, flag := range []string{"assume-unchanged", "skip-worktree"} {
		repoGit(t, dir, "update-index", "--"+flag, "base")
		if _, err := Snapshot(t.Context(), dir); err == nil {
			t.Fatal("隐藏改动")
		}
		repoGit(t, dir, "update-index", "--no-"+flag, "base")
	}
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	repoFile(t, dir, "base", "再改")
	takeSnapshot(t, dir)
	after, err := os.ReadFile(index)
	if err != nil || string(before) != string(after) {
		t.Fatal("改写索引", err)
	}
}

func TestSnapshotRenameLinkDelete(t *testing.T) {
	dir := testRepo(t)
	clean := takeSnapshot(t, dir)
	repoGit(t, dir, "mv", "--", "base", "moved")
	renamed := takeSnapshot(t, dir)
	if err := os.Remove(filepath.Join(dir, "moved")); err != nil {
		t.Fatal(err)
	}
	removed := takeSnapshot(t, dir)
	if renamed == removed || clean == removed || clean == renamed {
		t.Fatal("改名和删除未区分")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("../outside", link); err != nil {
		t.Skipf("无法创建符号链接：%v", err)
	}
	first := takeSnapshot(t, dir)
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../elsewhere", link); err != nil {
		t.Fatal(err)
	}
	if first == takeSnapshot(t, dir) {
		t.Fatal("链接目标未绑定")
	}
}

func TestSnapshotStreamsWholeFile(t *testing.T) {
	dir := testRepo(t)
	name := filepath.Join(dir, "large")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 20); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before := takeSnapshot(t, dir)
	f, err = os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{1}, (4<<20)-1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if before == takeSnapshot(t, dir) {
		t.Fatal("文件尾部变动未进入快照")
	}
}
