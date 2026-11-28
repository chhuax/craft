package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/method"
)

func (m *methodCLI) gitOutput(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-optional-locks", "--literal-pathspecs", "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = m.root.Name()
	raw, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s：%w", args[0], err)
	}
	return string(raw), nil
}

func (m *methodCLI) tasks() (result, error) {
	text, err := m.store.LoadTasks(m.change.ID)
	if err != nil {
		return result{}, err
	}
	var tasks []method.Task
	if text != "" {
		tasks, err = method.ParseTasks(text, m.requirement().Scenarios)
		if err != nil {
			return result{}, methodRefused(err)
		}
	}
	var commits []method.Commit
	if m.change.TasksFile != "" || len(tasks) > 0 {
		base, err := method.BaseCommit(m.ctx, m.root.Name(), frontText(m.requirement(), "baseBranch"))
		if err != nil {
			return data(map[string]any{"change": m.change.ID, "tasksFile": m.change.TasksFile, "total": len(tasks), "done": nil, "remaining": nil, "doneError": err.Error()})
		}
		commits, err = method.Commits(m.ctx, m.root.Name(), base)
		if err != nil {
			return result{}, err
		}
	}
	progress := method.Continuation(m.change.ID, tasks, commits)
	// 展示波次时按不区分大小写的文件系统保守合并，避免给出跨平台的文件冲突。
	waves, err := method.Waves(tasks, progress.Done, true)
	if err != nil {
		return result{}, err
	}
	return data(struct {
		method.Progress
		Change string     `json:"change"`
		File   string     `json:"tasksFile,omitempty"`
		Total  int        `json:"total"`
		Waves  [][]string `json:"waves"`
	}{progress, m.change.ID, m.change.TasksFile, len(tasks), waves})
}

func (m *methodCLI) tier() (result, error) {
	req := m.requirement()
	facts, err := method.DocumentRisk(req, m.design())
	if err != nil {
		return result{}, methodRefused(err)
	}
	ref := m.flags["--base"]
	if ref == "" {
		ref = frontText(req, "baseBranch")
	}
	base, baseErr := method.BaseCommit(m.ctx, m.root.Name(), ref)
	paths, err := method.WorktreeStatus(m.ctx, m.root.Name())
	if err != nil {
		return result{}, err
	}
	var changed []string
	for _, p := range paths {
		changed = append(changed, p.Path)
	}
	if baseErr == nil {
		out, err := m.gitOutput("diff", "--name-only", "-z", "--no-renames", base, "--")
		if err != nil {
			return result{}, err
		}
		changed = append(changed, strings.Split(out, "\x00")...)
	}
	slices.Sort(changed)
	var hits []string
	for _, file := range slices.Compact(changed) {
		if file == "" || strings.HasPrefix(file, methodChanges+"/") || strings.HasPrefix(file, methodStateDir+"/") {
			continue
		}
		base := path.Base(file)
		parts := "/" + file
		switch {
		case strings.HasSuffix(file, ".proto"), strings.Contains(parts, "/api/"), strings.Contains(base, "Controller"):
			facts.Interface = true
		case strings.HasSuffix(file, ".sql"), strings.Contains(parts, "/db/"), strings.Contains(parts, "/migration"):
			facts.Schema = true
		case strings.HasSuffix(file, ".yml"), strings.HasSuffix(file, ".yaml"), strings.HasSuffix(file, ".properties"):
			facts.Config = true
		default:
			continue
		}
		hits = append(hits, file)
	}
	tier, reasons := method.SuggestTier(facts)
	payload := map[string]any{"change": m.change.ID, "current": m.change.Tier, "suggested": tier, "paths": hits}
	if baseErr != nil {
		payload["baseError"] = baseErr.Error()
	}
	r := result{payload: payload}
	for _, reason := range reasons {
		r.evidence = append(r.evidence, reason)
	}
	return r, nil
}
