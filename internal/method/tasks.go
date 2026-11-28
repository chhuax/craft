package method

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Task 对应任务清单的一节。Parallel 表示允许同波，实际还要核对依赖和目标文件。
type Task struct {
	ID, Description, Signature, Test string
	Depends, Targets, Scenarios      []string
	Parallel                         bool
}

var taskID = regexp.MustCompile("^[a-z0-9]+(-[a-z0-9]+)*$")
var quotedItem = regexp.MustCompile("`([^`]+)`")

func listItems(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r == ',' || r == '，' || r == '、' })
}

// ParseTasks 解析带 craft 文件头或旧式无头清单，核对依赖和场景覆盖。scenarios 为 nil 时只查编号格式。
func ParseTasks(text string, scenarios []Scenario) ([]Task, error) {
	return parseTaskDocument(Parse("tasks.md", text), scenarios)
}

func parseTaskDocument(doc Document, scenarios []Scenario) ([]Task, error) {
	if len(doc.Issues) > 0 {
		return nil, fmt.Errorf("任务清单第 %d 行：%s", doc.Issues[0].Line, doc.Issues[0].Message)
	}
	if doc.Front != nil && doc.value("craft") != formatVersion {
		return nil, fmt.Errorf("任务清单文件头缺少版本")
	}
	if len(doc.Sections) == 0 {
		return nil, fmt.Errorf("任务清单为空")
	}
	known := map[string]bool{}
	for _, s := range scenarios {
		known[s.ID] = true
	}
	covered := map[string]bool{}
	var tasks []Task
	for _, section := range doc.Sections {
		task, err := readTask(section)
		if err != nil {
			return nil, fmt.Errorf("第 %d 行任务 %q：%w", section.Line, section.Name, err)
		}
		for _, id := range task.Scenarios {
			if scenarios != nil && !known[id] {
				return nil, fmt.Errorf("任务 %s 引用了未知场景 %s", task.ID, id)
			}
			covered[id] = true
		}
		tasks = append(tasks, task)
	}
	for _, s := range scenarios {
		if !covered[s.ID] {
			return nil, fmt.Errorf("场景 %s 没有任务负责", s.ID)
		}
	}
	if err := checkTaskGraph(tasks); err != nil {
		return nil, err
	}
	return tasks, nil
}

func readTask(section Section) (Task, error) {
	fields, err := taskFields(section.Content)
	if err != nil {
		return Task{}, err
	}
	for _, name := range []string{"依赖", "目标文件", "对应", "说明", "验证", "接口"} {
		if fields[name] == "" {
			return Task{}, fmt.Errorf("缺少%s", name)
		}
	}
	task := Task{ID: section.Name, Description: fields["说明"], Signature: fields["接口"], Test: fields["验证"], Scenarios: listItems(fields["对应"]), Parallel: true}
	if !taskID.MatchString(task.ID) || quotedItem.FindString(task.Test) == "" {
		return Task{}, fmt.Errorf("任务编号或验证命令无效")
	}
	for _, id := range task.Scenarios {
		if !scenarioID.MatchString(id) {
			return Task{}, fmt.Errorf("未知场景编号 %q", id)
		}
	}
	if len(task.Scenarios) == 0 {
		return Task{}, fmt.Errorf("缺少场景编号")
	}
	if fields["依赖"] != "无" {
		for _, id := range listItems(fields["依赖"]) {
			task.Depends = append(task.Depends, strings.Trim(id, "`"))
		}
		if len(task.Depends) == 0 {
			return Task{}, fmt.Errorf("缺少依赖编号")
		}
	}
	switch fields["可并行"] {
	case "", "是":
	case "否":
		task.Parallel = false
	default:
		return Task{}, fmt.Errorf("可并行标记无效")
	}
	task.Targets, err = taskTargets(fields["目标文件"])
	return task, err
}

func taskFields(content string) (map[string]string, error) {
	var code bool
	fields := map[string]string{}
	for line := range strings.SplitSeq(content, "\n") {
		if fenced(line) {
			code = !code
			continue
		}
		item, ok := strings.CutPrefix(strings.TrimSpace(line), "-")
		if code || !ok {
			continue
		}
		i := strings.IndexAny(item, ":：")
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(item[:i])
		_, width := utf8.DecodeRuneInString(item[i:])
		val := item[i+width:]
		if _, ok := fields[key]; ok {
			return nil, fmt.Errorf("字段 %q 重复", key)
		}
		fields[key] = strings.TrimSpace(val)
	}
	return fields, nil
}

func taskTargets(files string) ([]string, error) {
	var targets []string
	seen := map[string]bool{}
	var names []string
	if strings.Contains(files, "`") {
		for _, m := range quotedItem.FindAllStringSubmatch(files, -1) {
			names = append(names, m[1])
		}
		if rest := listItems(quotedItem.ReplaceAllString(files, "")); len(rest) > 0 {
			return nil, fmt.Errorf("目标文件引号或分隔符无效")
		}
	} else {
		names = listItems(files)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("缺少目标文件")
	}
	for _, file := range names {
		name, err := targetName(file)
		if err != nil {
			return nil, err
		}
		if !seen[name] {
			targets = append(targets, name)
			seen[name] = true
		}
	}
	return targets, nil
}

func targetName(file string) (string, error) {
	file = strings.ReplaceAll(strings.TrimSpace(file), "\\", "/")
	clean := path.Clean(file)
	if strings.HasSuffix(file, "/") || path.Base(file) == "." || path.Base(file) == ".." || !safePath(clean) || strings.ContainsAny(clean, "[]{}") {
		return "", fmt.Errorf("目标不是具体的项目内文件：%q", file)
	}
	return clean, nil
}

func checkTaskGraph(tasks []Task) error {
	graph := map[string][]string{}
	for _, t := range tasks {
		if _, exists := graph[t.ID]; exists || !taskID.MatchString(t.ID) {
			return fmt.Errorf("任务编号重复或无效：%q", t.ID)
		}
		graph[t.ID] = t.Depends
	}
	seen := map[string]bool{}
	var visit func(string) error
	visit = func(id string) error {
		deps, ok := graph[id]
		if !ok {
			return fmt.Errorf("依赖任务 %q 不存在", id)
		}
		if done, visited := seen[id]; visited {
			if !done {
				return fmt.Errorf("任务 %q 存在循环依赖", id)
			}
			return nil
		}
		seen[id] = false
		for _, d := range deps {
			if err := visit(d); err != nil {
				return err
			}
		}
		seen[id] = true
		return nil
	}
	for _, t := range tasks {
		if err := visit(t.ID); err != nil {
			return err
		}
	}
	return nil
}

// Waves 从已完成集合推演剩余任务。每波最多四项，按清单顺序选择，用同一波开始前的完成集合判断就绪。
func Waves(tasks []Task, done []string, ignoreCase bool) ([][]string, error) {
	if err := checkTaskGraph(tasks); err != nil {
		return nil, err
	}
	finished := map[string]bool{}
	for _, id := range done {
		finished[id] = true
	}
	files := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		if len(t.Targets) == 0 {
			return nil, fmt.Errorf("任务 %s 缺少目标文件", t.ID)
		}
		for _, f := range t.Targets {
			key, err := targetName(f)
			if err != nil {
				return nil, err
			}
			if ignoreCase {
				key = strings.ToLower(key)
			}
			files[t.ID] = append(files[t.ID], key)
		}
	}
	waves := [][]string{}
	for {
		var wave []string
		claimed := map[string]bool{}
		for _, t := range tasks {
			if finished[t.ID] || slices.ContainsFunc(t.Depends, func(dep string) bool { return !finished[dep] }) {
				continue
			}
			if (!t.Parallel && len(wave) > 0) || slices.ContainsFunc(files[t.ID], func(f string) bool { return claimed[f] }) {
				continue
			}
			wave = append(wave, t.ID)
			for _, f := range files[t.ID] {
				claimed[f] = true
			}
			if !t.Parallel || len(wave) == 4 {
				break
			}
		}
		if len(wave) == 0 {
			return waves, nil
		}
		for _, id := range wave {
			finished[id] = true
		}
		waves = append(waves, wave)
	}
}

// Progress 的完成依据仅为本次变更带 Task 行的提交。EmptyRange 提醒基础分支可能已合入工作，清单缺失时可用 Committed 重建任务编号。
type Progress struct {
	Version    int               `json:"version"`
	EmptyRange bool              `json:"emptyRange"`
	Committed  map[string]string `json:"committed"`
	Done       []string          `json:"done"`
	Remaining  []string          `json:"remaining"`
}

// Continuation 从 Commits 返回的历史恢复进度，同一任务保留第一次提交，完成与剩余均按清单顺序。
func Continuation(change string, tasks []Task, commits []Commit) Progress {
	p := Progress{
		Version: formatVersion, EmptyRange: len(commits) == 0, Committed: map[string]string{},
		Done: []string{}, Remaining: []string{},
	}
	prefix := change + "/"
	for _, commit := range commits {
		for line := range strings.SplitSeq(commit.Message, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok || !strings.EqualFold(key, "Task") {
				continue
			}
			for _, item := range listItems(value) {
				id, match := strings.CutPrefix(item, prefix)
				if match && taskID.MatchString(id) && p.Committed[id] == "" {
					p.Committed[id] = commit.ID
				}
			}
		}
	}
	for _, task := range tasks {
		if _, ok := p.Committed[task.ID]; ok {
			p.Done = append(p.Done, task.ID)
		} else {
			p.Remaining = append(p.Remaining, task.ID)
		}
	}
	return p
}
