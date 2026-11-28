package method

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func card(id, dep, file string) string {
	return fmt.Sprintf("## %s\n- 依赖：%s\n- 目标文件：`%s`\n- 接口：`Read() string`\n- 说明：读文件\n- 对应：S-1、S-2\n- 验证：`go test ./reader` → 两例通过\n", id, dep, file)
}

func TestTaskList(t *testing.T) {
	req := Parse("req.md", scenarios)
	good := card("read", "无", "src/read.go")
	for _, colon := range []string{"：", ":"} {
		text := strings.ReplaceAll(good+"- 可并行：否\n- 状态：完成\n", "：", colon)
		tasks, err := ParseTasks(strings.ReplaceAll(text, "\n", "\r\n"), req.Scenarios)
		if err != nil || len(tasks) != 1 {
			t.Fatal(tasks, err)
		}
		a := tasks[0]
		if a.ID != "read" || !slices.Equal(a.Targets, []string{"src/read.go"}) || !slices.Equal(a.Scenarios, []string{"S-1", "S-2"}) || !strings.Contains(a.Signature, "Read() string") || !strings.Contains(a.Test, "go test") || a.Parallel {
			t.Fatal(a)
		}
	}
	for _, text := range []string{"", good + good, strings.Replace(good, "## read", "## Read!", 1), strings.Replace(good, "S-1、S-2", "S-9", 1), strings.Replace(good, "S-1、S-2", "S-2", 1), card("read", "ghost", "a"), card("read", "read", "a"), card("a", "b", "a") + card("b", "a", "b"), card("read", "无", "src/"), card("read", "无", "../a"), card("read", "无", "*.go"), strings.Replace(good, "- 验证：", "- 其他：", 1), "---\ncraft: 2\n---\n" + good, good + "- 可并行：不清楚\n"} {
		if _, err := ParseTasks(text, req.Scenarios); err == nil {
			t.Error(text)
		}
	}
}

func TestTaskWaves(t *testing.T) {
	for _, tc := range []struct {
		text string
		done []string
		fold bool
		want [][]string
	}{
		{card("a", "无", "a") + card("b", "a", "b") + card("c", "a", "c") + card("d", "b、c", "d"), []string{"a"}, false, [][]string{{"b", "c"}, {"d"}}},
		{card("a", "无", "src/X") + card("b", "无", "./src\\x") + card("c", "无", "z"), nil, true, [][]string{{"a", "c"}, {"b"}}},
		{card("a", "无", "a") + card("b", "无", "b") + "- 可并行：否\n" + card("c", "无", "c"), nil, false, [][]string{{"a", "c"}, {"b"}}},
		{card("a", "无", "a") + card("b", "无", "b") + card("c", "无", "c") + card("d", "无", "d") + card("e", "无", "e"), nil, false, [][]string{{"a", "b", "c", "d"}, {"e"}}},
	} {
		tasks, err := ParseTasks(tc.text, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Waves(tasks, tc.done, tc.fold)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Error(got, err)
		}
	}
}

func TestContinuation(t *testing.T) {
	dir := testRepo(t)
	tasks, err := ParseTasks(card("old", "无", "a")+"- 状态：完成\n"+card("new", "无", "b")+card("next", "new", "c"), nil)
	if err != nil {
		t.Fatal(err)
	}
	repoGit(t, dir, "commit", "--allow-empty", "-qm", "一\n\nTask: change/new\n\n后续段落")
	first := repoGit(t, dir, "rev-parse", "HEAD")
	repoGit(t, dir, "commit", "--allow-empty", "-qm", "提到 Task: change/next\n\nTask: other/old, next\nTaSk: change/new，change/unlisted")
	base, err := BaseCommit(t.Context(), dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	commits, err := Commits(t.Context(), dir, base)
	if err != nil || len(commits) != 2 {
		t.Fatal(commits, err)
	}
	p := Continuation("change", tasks, commits)
	if p.Version != 1 || !slices.Equal(p.Done, []string{"new"}) || !slices.Equal(p.Remaining, []string{"old", "next"}) || len(p.Committed) != 2 || p.Committed["new"] != first {
		t.Fatal(p)
	}
	empty, err := Commits(t.Context(), dir, repoGit(t, dir, "rev-parse", "HEAD"))
	if err != nil || !Continuation("change", tasks, empty).EmptyRange {
		t.Fatal(empty, err)
	}
	if p := Continuation("change", nil, commits); len(p.Committed) != 2 {
		t.Fatal(p)
	}
}
