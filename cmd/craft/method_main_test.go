package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// 用实际进程入口覆盖角色识别、参数、信封及退出码；子进程只接受 method 命令。
func TestMethodProcess(t *testing.T) {
	if os.Getenv("CRAFT_TEST_METHOD") != "1" {
		return
	}
	at := slices.Index(os.Args, "--")
	if at < 0 || len(os.Args) <= at+1 || os.Args[at+1] != "method" {
		t.Fatal("缺少 method 命令")
	}
	os.Args = append([]string{"craft"}, os.Args[at+1:]...)
	main()
}

type methodRepo struct {
	t   *testing.T
	dir string
}

func newMethodRepo(t *testing.T) methodRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("没有 git")
	}
	// 继承的索引、对象库或配置都可能把 git 写操作引向测试仓库之外。
	for _, entry := range os.Environ() {
		k, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(k), "GIT_") {
			t.Setenv(k, "")
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_DATE", "2000-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2000-01-01T00:00:00Z")
	r := methodRepo{t, t.TempDir()}
	t.Setenv("CRAFT_HOME", filepath.Join(t.TempDir(), "unused"))
	t.Setenv("CRAFT_TEST_METHOD", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	r.git("init", "-q", "--template=", "-b", "main")
	r.git("config", "core.autocrlf", "false")
	r.git("config", "user.name", "Example")
	r.git("config", "user.email", "example@example.invalid")
	r.git("commit", "-q", "--allow-empty", "-m", "起点")
	r.git("checkout", "-q", "-b", "work")
	return r
}

func (r methodRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", args...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r methodRepo) call(want int, into any, args ...string) {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), os.Args[0], append([]string{"-test.run=^TestMethodProcess$", "--", "method"}, append(args, "--json")...)...)
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		e, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			r.t.Fatal(err)
		}
		code = e.ExitCode()
	}
	var env struct {
		OK       bool
		Payload  json.RawMessage
		Evidence []json.RawMessage
		Error    struct {
			Code   string
			Detail json.RawMessage
		}
	}
	if code != want || json.Unmarshal(out, &env) != nil || env.OK != (want == 0) {
		r.t.Fatalf("method %v: %d %s", args, code, out)
	}
	value := env.Payload
	if want != 0 {
		value = env.Error.Detail
		if env.Error.Code == "" || value == nil {
			r.t.Fatalf("错误信封 %s", out)
		}
	} else if env.Evidence == nil || value == nil {
		r.t.Fatalf("成功信封 %s", out)
	}
	if into != nil {
		if err := json.Unmarshal(value, into); err != nil {
			r.t.Fatal(err)
		}
	}
}

func (r methodRepo) write(name, body string) {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, filepath.FromSlash(name)), []byte(body), 0600); err != nil {
		r.t.Fatal(err)
	}
}

func TestMethodStandalone(t *testing.T) {
	r := newMethodRepo(t)
	const intent = "导出本地标签时，列表应按名称升序，也能只看匹配的标签。"
	r.git("tag", "beta")
	r.git("tag", "alpha")
	r.git("config", "tag.sort", "-refname")
	r.git("config", "include.path", "../names.conf")
	var created struct{ Change, Dir, TasksFile string }
	r.call(0, &created, "new", "--project", r.dir, "--name", "sort-names", "--type", "feature", "--tier", "standard", "--base-branch", "main")
	req := filepath.Join(created.Dir, "requirements.md")
	design := filepath.Join(created.Dir, "design.md")
	r.write(req, "---\ncraft: 1\ndescription: 按名称排序\ntype: feature\ntier: standard\nbaseBranch: main\n---\n## 背景\n"+intent+"\n## 目标与范围\n仅处理导出。\n## 业务规则\n先显示字典序较小的名称。\n## 验收场景\n| 编号 | 场景 | 预期 |\n|---|---|---|\n| S-1 | 已有 beta 和 alpha | 先显示 alpha |\n| S-2 | 筛选 alpha | 仅显示 alpha |\n")
	const plan = "---\ncraft: 1\n---\n## 结论\n导出前排序。\n## 设计\n列标签时指定升序。\n## 考虑过的其他做法\n修改默认排序会影响其他查询。\n## 验证\n| 场景 | 证据 | 方式 |\n|---|---|---|\n"
	r.write(design, plan)
	var gaps struct {
		Problems []struct {
			Rule, Path string
			Line       int
		}
	}
	r.call(5, &gaps, "check", "--for", "exit")
	if len(gaps.Problems) != 1 || gaps.Problems[0].Rule != "F6 coverage" || filepath.Base(gaps.Problems[0].Path) != "design.md" || gaps.Problems[0].Line < 1 {
		t.Fatal(gaps)
	}
	r.write(design, plan+"| S-1 | check-order | 自动 |\n| S-2 | check-match | 自动 |\n")
	r.call(0, nil, "check", "--for", "exit")
	var confirmed struct {
		Confirmation struct {
			By   string
			Docs []struct{ Hash string }
		}
	}
	r.call(0, &confirmed, "confirm", "--by", "作者")
	if confirmed.Confirmation.By != "作者" || len(confirmed.Confirmation.Docs) != 2 || len(confirmed.Confirmation.Docs[0].Hash) != 64 {
		t.Fatal(confirmed)
	}
	r.write(created.TasksFile, "---\ncraft: 1\n---\n## sort\n- 依赖：无\n- 目标文件：names.conf\n- 对应：S-1\n- 说明：导出升序标签\n- 接口：每行一个名称\n- 验证：`git names`\n\n## match\n- 依赖：sort\n- 目标文件：names.conf\n- 对应：S-2\n- 说明：按模式列出匹配项\n- 接口：匹配的名称列表\n- 验证：`git matching alpha`\n")
	var progress struct {
		Done, Remaining []string
		Committed       map[string]string
	}
	r.call(0, &progress, "tasks")
	if !slices.Equal(progress.Remaining, []string{"sort", "match"}) || len(progress.Done) != 0 {
		t.Fatal(progress)
	}
	const alias = "[alias]\n names = tag --list --sort=refname\n"
	r.write("names.conf", alias)
	if got := r.git("names"); strings.Join(strings.Fields(got), ",") != "alpha,beta" {
		t.Fatalf("名称顺序：%q", got)
	}
	r.git("add", "names.conf")
	r.git("commit", "-qm", "导出按名称排序", "-m", "Task: "+created.Change+"/sort")
	r.call(0, &progress, "tasks", "--change", "sort-names")
	if !slices.Equal(progress.Remaining, []string{"match"}) || !slices.Equal(progress.Done, []string{"sort"}) || progress.Committed["sort"] != r.git("rev-parse", "HEAD") {
		t.Fatal(progress)
	}
	const implementation = alias + " matching = names\n"
	r.write("names.conf", implementation)
	if got := r.git("matching", "alpha"); got != "alpha" {
		t.Fatal(got)
	}
	r.git("add", "names.conf")
	r.git("commit", "-qm", "支持按标签名称筛选", "-m", "Task: "+created.Change+"/match")
	r.call(0, &progress, "tasks")
	if len(progress.Remaining) != 0 || !slices.Equal(progress.Done, []string{"sort", "match"}) {
		t.Fatal(progress)
	}
	// 使用 git 本身作为样例项目的执行器与比较器，收尾命令能对错误的排序给出非零退出码。
	r.write(".craft/method/expected", "alpha\nbeta\nalpha\n")
	const guard = "git names > .craft/method/actual && git matching alpha >> .craft/method/actual && git diff --no-index --ignore-space-at-eol --exit-code .craft/method/expected .craft/method/actual"
	var run struct {
		OK, Reused bool
		Record     struct {
			Before, After string
			Exit          *int
		}
	}
	r.call(0, &run, "record", "--run", guard)
	if !run.OK || run.Record.Exit == nil || *run.Record.Exit != 0 || len(run.Record.Before) != 64 || run.Record.After != run.Record.Before {
		t.Fatal(run)
	}
	r.call(0, &run, "record", "--run", guard, "--reuse")
	if !run.Reused {
		t.Fatal(run)
	}
	report := filepath.Join(t.TempDir(), "review.json")
	if err := os.WriteFile(report, []byte(`{"version":1,"scenarios":[{"id":"S-1","result":"pass","evidence":"git names: alpha, beta"},{"id":"S-2","result":"pass","evidence":"git matching alpha: alpha"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	r.call(0, nil, "verify", "--by", "复核人", "--report", report)
	var facts struct {
		Closing, Review string
		Confirmation    struct{ State string }
	}
	r.call(0, &facts, "evidence")
	if facts.Closing != "pass" || facts.Review != "pass" || facts.Confirmation.State != "confirmed" {
		t.Fatal(facts)
	}
	r.write("names.conf", strings.Replace(implementation, "=refname", "=-refname", 1))
	r.call(0, &facts, "evidence")
	if facts.Closing != "stale" || facts.Review != "stale" || facts.Confirmation.State != "confirmed" {
		t.Fatal(facts)
	}
	var note struct{ Text string }
	r.call(0, &note, "note")
	if !strings.Contains(note.Text, intent) || !strings.Contains(note.Text, "S-1") || !strings.Contains(note.Text, "check-order") {
		t.Fatal(note.Text)
	}
	r.call(5, nil, "verify", "--by", "复核人", "--report", report)
	r.call(0, &run, "record", "--run", guard, "--reuse")
	if run.OK || run.Reused || run.Record.Exit == nil || *run.Record.Exit == 0 {
		t.Fatal(run)
	}
	if strings.Contains(r.git("status", "--porcelain"), ".craft") {
		t.Fatal("状态文件被 git 收集")
	}
	if _, err := os.Stat(os.Getenv("CRAFT_HOME")); !os.IsNotExist(err) {
		t.Fatalf("访问了全局目录: %v", err)
	}
}

func TestMethodDirectChange(t *testing.T) {
	r := newMethodRepo(t)
	t.Setenv("CODEX_THREAD_ID", "isolated")
	t.Setenv("CRAFT_WATCH_ROLE", "worker")
	t.Setenv("CRAFT_WORK_ID", "sample")
	t.Setenv("CRAFT_ASSIGNMENT_ID", "sample")
	r.call(0, nil, "fact", "--change", "quick-fix", "--text", "已复现")
	r.call(5, nil, "check", "--change", "quick-fix", "--for", "exit")
	r.call(0, nil, "fact", "--change", "quick-fix", "--text", "design-skipped")
	var out struct {
		OK       bool
		Problems []json.RawMessage
	}
	r.call(0, &out, "check", "--change", "quick-fix", "--for", "exit")
	if !out.OK || len(out.Problems) != 0 {
		t.Fatal(out)
	}
}

func TestMethodArchive(t *testing.T) {
	r := newMethodRepo(t)
	var change struct{ Change, Dir string }
	r.call(0, &change, "new", "--name", "catalog", "--type", "feature", "--tier", "standard", "--draft", "--item", "sample", "--capability", "目录", "--base-branch", "main")
	if !strings.HasPrefix(change.Dir, ".craft/") {
		t.Fatal(change)
	}
	plan := filepath.Join(change.Dir, "design.md")
	const delta = "## 接口与库表变更\n### 目录\n#### 新增 GET /names\n返回名称。\n#### 新增 name_index\n名称唯一。\n"
	r.write(plan, delta)
	var tier struct{ Suggested string }
	r.call(0, &tier, "tier")
	if tier.Suggested != "heavy" {
		t.Fatal(tier)
	}
	var matched struct{ Changes []struct{ Change string } }
	r.call(0, &matched, "status", "--list", "--item", "sample")
	if len(matched.Changes) != 1 || matched.Changes[0].Change != change.Change {
		t.Fatal(matched)
	}
	var archived struct{ Files []string }
	r.call(0, &archived, "archive")
	if len(archived.Files) != 2 {
		t.Fatal(archived)
	}
	base := make(map[string]string)
	for _, name := range archived.Files {
		raw, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		base[name] = string(raw)
	}
	r.call(0, &archived, "archive")
	if len(archived.Files) != 0 {
		t.Fatal(archived)
	}
	r.write(plan, strings.ReplaceAll(delta, "返回名称", "按拼音展示")+"#### 修改 name_order\n顺序唯一。\n")
	r.call(5, nil, "archive")
	for name, want := range base {
		raw, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(name)))
		if err != nil || string(raw) != want {
			t.Fatalf("冲突后 %s 已改变：%v", name, err)
		}
	}
}

func TestMethodDraftConfirmation(t *testing.T) {
	r := newMethodRepo(t)
	var draft struct {
		Files  []string
		Change string
	}
	r.call(0, &draft, "new", "--name", "summary", "--tier", "light", "--type", "explore", "--draft")
	var hashes struct{ Files []struct{ Path, Hash string } }
	r.call(0, &hashes, "hash", "--file", draft.Files[0])
	var confirmed struct{ Confirmation json.RawMessage }
	r.call(0, &confirmed, "confirm", "--by", "作者")
	var docs struct{ Docs []struct{ Path, Hash string } }
	if err := json.Unmarshal(confirmed.Confirmation, &docs); err != nil || !slices.Equal(docs.Docs, hashes.Files) || len(docs.Docs) != 1 {
		t.Fatalf("摘要不一致：%s %v", confirmed.Confirmation, err)
	}
	jsonFile := filepath.Join(t.TempDir(), "confirmed.json")
	if err := os.WriteFile(jsonFile, confirmed.Confirmation, 0600); err != nil {
		t.Fatal(err)
	}
	var resumed struct {
		Change string
		Files  []string
		Reused bool
	}
	r.call(0, &resumed, "new", "--name", "summary", "--tier", "light", "--type", "explore", "--change", draft.Change)
	if !resumed.Reused || resumed.Change != draft.Change || resumed.Files[0] == draft.Files[0] {
		t.Fatal(resumed)
	}
	var state struct {
		State string
		Docs  []struct{ State, MovedTo string }
	}
	r.call(0, &state, "hash", "--check", jsonFile)
	if state.State != "confirmed" || len(state.Docs) != 1 || state.Docs[0].State != "moved" || state.Docs[0].MovedTo != resumed.Files[0] {
		t.Fatal(state)
	}
	var flow struct {
		Flow []struct{ Name, State string }
	}
	r.call(0, &flow, "flow")
	if len(flow.Flow) == 0 || flow.Flow[len(flow.Flow)-1].State != "done" {
		t.Fatal(flow)
	}
	r.write(resumed.Files[0], "---\ncraft: 1\ntype: explore\ntier: light\n---\n## 结论\n新发现\n")
	r.call(0, &flow, "flow")
	if flow.Flow[len(flow.Flow)-1].State != "stale" {
		t.Fatal(flow)
	}
}
