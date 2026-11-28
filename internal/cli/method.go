package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/method"
)

const methodStateDir = ".craft/method"
const methodChanges = "docs/changes"
const methodDrafts = methodStateDir + "/drafts"

func methodCommands() []commandSpec {
	const project = "[--project DIR]"
	const change = project + " [--change ID]"
	return []commandSpec{
		{"method paths", "", project, local, runMethod},
		{"method hash", "", project + " [--file FILE...] [--check FILE] [--also FILE...]", local, runMethod},
		{"method snapshot", "", project, local, runMethod},
		{"method report check", "", project + " --file FILE --scenarios IDS", local, runMethod},
		{"method new", "", change + " --name NAME --type TYPE --tier TIER [--group GROUP] [--item ID...] [--base-branch REF] [--capability NAME...] [--draft] [--new-round]", local, runMethod},
		{"method status", "", change + " [--list] [--item ID]", local, runMethod},
		{"method flow", "", change, local, runMethod},
		{"method check", "", change + " [--for LEVEL]", local, runMethod},
		{"method confirm", "", change + " --by NAME", local, runMethod},
		{"method tasks", "", change, local, runMethod},
		{"method tier", "", change + " [--base REF]", local, runMethod},
		{"method record", "", change + " [--run CMD] [--timeout SEC] [--reuse] [--cmd TEXT] [--exit CODE]", local, runMethod},
		{"method verify", "", change + " --by NAME --report FILE", local, runMethod},
		{"method evidence", "", change, local, runMethod},
		{"method note", "", change + " [--out FILE]", local, runMethod},
		{"method archive", "", change, local, runMethod},
		{"method fact", "", change + " --text TEXT", local, runMethod},
	}
}

type methodCLI struct {
	*invocation
	root   *os.Root
	store  method.StateStore
	now    time.Time
	change methodChange

	log []method.Record
}

type methodRefusal struct {
	cause  error
	detail any
}

func (e *methodRefusal) Error() string { return e.cause.Error() }
func (e *methodRefusal) Unwrap() error { return e.cause }

func methodRefused(err error) error {
	return &methodRefusal{err, map[string]string{}}
}

func runMethod(i *invocation) (_ result, err error) {
	dir, err := filepath.Abs(i.flags["--project"])
	if err != nil {
		return result{}, fmt.Errorf("定位项目：%w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return result{}, fmt.Errorf("打开项目 %q：%w", dir, err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	store, err := method.NewFileStore(dir, "")
	if err != nil {
		return result{}, err
	}
	now, err := time.Parse(time.RFC3339Nano, i.clock())
	if err != nil {
		return result{}, fmt.Errorf("读取命令时钟：%w", err)
	}
	m := methodCLI{invocation: i, root: root, store: store, now: now}
	res, err := m.execute()
	if errors.Is(err, exec.ErrNotFound) {
		err = fmt.Errorf("%w：%w", errDependency, err)
	} else if errors.Is(err, os.ErrNotExist) {
		err = fmt.Errorf("%w：%w", errMissing, err)
	}
	if err != nil {
		return result{}, fmt.Errorf("%s：%w", i.spec.path, err)
	}
	return res, nil
}

func (m *methodCLI) execute() (result, error) {
	switch m.spec.path {
	case "method paths":
		return data(map[string]string{"project": m.root.Name(), "stateDir": filepath.Join(m.root.Name(), methodStateDir), "ignoreHint": "状态目录使用自身的忽略规则。"})
	case "method hash":
		return m.hash()
	case "method snapshot":
		digest, err := method.Snapshot(m.ctx, m.root.Name())
		if err != nil {
			return result{}, err
		}
		return data(map[string]string{"snapshot": digest})
	case "method report check":
		return m.checkReport()
	}
	if m.spec.path == "method new" {
		return m.newChange()
	}
	changes, err := m.findChanges()
	if err != nil {
		return result{}, err
	}
	if m.spec.path == "method status" {
		if _, list := m.flags["--list"]; list {
			if m.flags["--change"] != "" {
				return result{}, invalid("--list 与 --change 冲突")
			}
			return data(map[string]any{"changes": changes})
		}

	}
	if len(changes) > 1 && m.flags["--change"] == "" && m.flags["--item"] == "" {
		active, err := m.activeChanges(changes)
		if err != nil {
			return result{}, err
		}
		if len(active) != 1 && m.spec.path == "method status" {
			return data(map[string]any{"changes": changes})
		}
		changes = active
	}
	if m.spec.path == "method status" && m.flags["--change"] == "" && len(changes) != 1 {
		return data(map[string]any{"changes": changes})
	}
	if err := m.selectChange(changes); err != nil {
		return result{}, err
	}
	return m.changeCommand()
}

func (m *methodCLI) changeCommand() (result, error) {
	switch m.spec.path {
	case "method fact":
		fact := strings.TrimSpace(m.flags["--text"])
		if err := m.store.RecordFact(m.change.ID, m.now, fact); err != nil {
			return result{}, err
		}
		return data(map[string]string{"change": m.change.ID, "fact": fact})
	case "method tasks":
		return m.tasks()
	case "method tier":
		return m.tier()
	case "method archive":
		return m.archive()
	case "method record":
		return m.record()
	case "method verify":
		return m.verify()
	}
	log, err := m.store.LoadLedger(m.change.ID)
	if err != nil {
		return result{}, err
	}
	m.log = log
	skipped := slices.ContainsFunc(log, func(r method.Record) bool { return r.Fact == "design-skipped" })
	switch m.spec.path {
	case "method check":
		return m.check(skipped)
	case "method confirm":
		c, err := method.Confirm(m.flags["--by"], m.now, m.documents()...)
		if err != nil {
			return result{}, methodRefused(err)
		}
		if err := m.store.Save(m.change.ID, *c); err != nil {
			return result{}, err
		}
		return data(map[string]any{"change": m.change.ID, "confirmation": c, "warnings": method.CheckExit(m.change.docs, skipped)})
	case "method note":
		return m.note()
	case "method evidence":
		return m.evidence()
	}
	c, err := m.store.Load(m.change.ID)
	if err != nil {
		return result{}, err
	}
	stages := method.Flow(m.change.Kind, m.change.docs, c, skipped)
	if m.spec.path == "method flow" {
		return data(map[string]any{"change": m.change.ID, "flow": stages, "designSkipped": skipped})
	}
	progress, err := m.tasks()
	if err != nil {
		return result{}, err
	}
	return data(struct {
		methodChange
		Tasks    any            `json:"tasks"`
		Flow     []method.Stage `json:"flow"`
		Problems []method.Gap   `json:"problems"`
		Skipped  bool           `json:"designSkipped"`
	}{m.change, progress.payload, stages, method.CheckExit(m.change.docs, skipped), skipped})
}

func (m *methodCLI) check(skipped bool) (result, error) {
	level := m.flags["--for"]
	if level == "" {
		level = "draft"
	}
	if level != "draft" && level != "confirm" && level != "exit" {
		return result{}, invalid("--for 取 draft、confirm 或 exit")
	}
	var problems, notes []method.Gap
	for _, g := range method.CheckExit(m.change.docs, skipped) {
		if level == "draft" && g.Rule != "F1 front" {
			notes = append(notes, g)
		} else {
			problems = append(problems, g)
		}
	}
	payload := map[string]any{"change": m.change.ID, "for": level, "ok": len(problems) == 0, "problems": problems, "notes": notes}
	if level == "exit" && len(problems) > 0 {
		g := problems[0]
		return result{}, &methodRefusal{fmt.Errorf("文档有 %d 处缺口，%s:%d %s", len(problems), g.Path, g.Line, g.Message), payload}
	}
	return data(payload)
}

func (m *methodCLI) documents() []method.Document {
	var docs []method.Document
	for _, name := range methodDocNames {
		if d, ok := m.change.docs[name]; ok {
			docs = append(docs, d)
		}
	}
	return docs
}

func (m *methodCLI) requirement() method.Document {
	if d, ok := m.change.docs["change.md"]; ok {
		return d
	}
	return m.change.docs["requirements.md"]
}

func (m *methodCLI) design() method.Document {
	if d, ok := m.change.docs["change.md"]; ok {
		return d
	}
	return m.change.docs["design.md"]
}

// 新建变更须跨分组保证编号唯一，合并现状须从最新正文计算。两者共用项目内的文件锁，
// 进程退出时系统释放锁；保留锁文件可避免下一次调用锁住不同的文件。
func (m *methodCLI) lockDocuments() (*os.File, error) {
	if err := m.store.Prepare(); err != nil {
		return nil, err
	}
	name := filepath.Join(methodStateDir, "documents.lock")
	if info, err := m.root.Lstat(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	} else if err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("文档锁 %s 不是普通文件", name)
	}
	f, err := guardFile(filepath.Join(m.root.Name(), name))
	if err != nil {
		return nil, fmt.Errorf("取得项目文档锁：%w", err)
	}
	return f, nil
}
