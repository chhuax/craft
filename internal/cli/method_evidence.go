package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/method"
)

// 报告与确认记录是调用方交来的输入，路径按调用终端解释，短横线读标准输入。
func readMethodJSON(name string, into any) error {
	var raw []byte
	var err error
	if name == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(name)
	}
	if err != nil {
		return fmt.Errorf("读取 %q：%w", name, err)
	}
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")), into); err != nil {
		return invalid("解析 %s：%v", name, err)
	}
	return nil
}

func (m *methodCLI) repoPath(raw string) (string, error) {
	if filepath.IsAbs(raw) {
		rel, err := filepath.Rel(m.root.Name(), raw)
		if err != nil {
			return "", invalid("项目内路径 %q 无效", raw)
		}
		raw = rel
	}
	if !filepath.IsLocal(raw) {
		return "", invalid("路径 %q 超出项目", raw)
	}
	return filepath.ToSlash(filepath.Clean(raw)), nil
}

func (m *methodCLI) hash() (result, error) {
	check := m.flags["--check"]
	names := m.listFlags["--file"]
	if (len(names) == 0) == (check == "") || check == "" && len(m.listFlags["--also"]) != 0 {
		return result{}, invalid("使用 --file 或 --check，--also 仅用于核对")
	}
	if check != "" {
		var c method.Confirmation
		if err := readMethodJSON(check, &c); err != nil {
			return result{}, err
		}
		docs, err := m.hashCandidates(c)
		if err != nil {
			return result{}, err
		}
		return data(method.Verify(&c, docs...))
	}
	var files []method.ConfirmedDoc
	for _, name := range names {
		rel, err := m.repoPath(name)
		if err != nil {
			return result{}, err
		}
		raw, err := m.root.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return result{}, fmt.Errorf("读取 %s：%w", rel, err)
		}
		files = append(files, method.ConfirmedDoc{Path: rel, Hash: method.NormalizedHash(string(raw))})
	}
	return data(map[string]any{"files": files})
}

func (m *methodCLI) hashCandidates(c method.Confirmation) ([]method.Document, error) {
	var docs []method.Document
	seen := map[string]bool{}
	var missing []string
	read := func(name string) error {
		rel, err := m.repoPath(name)
		if err != nil {
			return err
		}
		if seen[rel] {
			return nil
		}
		raw, err := m.root.ReadFile(filepath.FromSlash(rel))
		if err != nil {
			return err
		}
		seen[rel] = true
		docs = append(docs, method.Parse(rel, string(raw)))
		return nil
	}
	for _, d := range c.Docs {
		if err := read(d.Path); errors.Is(err, fs.ErrNotExist) {
			missing = append(missing, filepath.Base(d.Path))
		} else if err != nil {
			return nil, err
		}
	}
	if len(missing) == 0 {
		return docs, nil
	}
	raw, err := m.gitOutput("ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, name := range append(strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00"), m.listFlags["--also"]...) {
		if slices.Contains(missing, filepath.Base(name)) {
			if err := read(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
		}
	}
	return docs, nil
}

func (m *methodCLI) checkReport() (result, error) {
	var report method.VerifierReport
	if err := readMethodJSON(m.flags["--file"], &report); err != nil {
		return result{}, err
	}
	req := method.Document{}
	for id := range strings.SplitSeq(m.flags["--scenarios"], ",") {
		req.Scenarios = append(req.Scenarios, method.Scenario{ID: strings.TrimSpace(id)})
	}
	if err := report.Check(req); err != nil {
		return result{}, invalid("报告：%v", err)
	}
	ok := report.Result() == "pass"
	return data(map[string]any{"ok": ok, "scenarios": report.Scenarios})
}

func (m *methodCLI) closingArgs() (method.ClosingRun, time.Duration, error) {
	auto, manual := m.flags["--run"], m.flags["--cmd"]
	exit, hasExit := m.flags["--exit"]
	if (auto == "") == (manual == "") || hasExit != (manual != "") {
		return method.ClosingRun{}, 0, invalid("使用 --run，或同时提供 --cmd 与 --exit")
	}
	_, reuse := m.flags["--reuse"]
	if reuse && auto == "" {
		return method.ClosingRun{}, 0, invalid("--reuse 只适用于 --run")
	}
	closing := method.ClosingRun{Command: manual, Automatic: auto != ""}
	if auto != "" {
		closing.Command = auto
	}
	limit := 15 * time.Minute
	if raw := m.flags["--timeout"]; raw != "" {
		duration, err := time.ParseDuration(raw + "s")
		if _, e := strconv.Atoi(raw); auto == "" || e != nil || err != nil || duration <= 0 {
			return method.ClosingRun{}, 0, invalid("--timeout 需要正整数秒和 --run")
		}
		limit = duration
	}
	if hasExit {
		code, err := strconv.Atoi(exit)
		if err != nil {
			return method.ClosingRun{}, 0, invalid("--exit 需要整数")
		}
		closing.Exit = &code
	}
	return closing, limit, nil
}

func (m *methodCLI) record() (result, error) {
	closing, limit, err := m.closingArgs()
	if err != nil {
		return result{}, err
	}
	before, err := method.Snapshot(m.ctx, m.root.Name())
	if err != nil {
		return result{}, err
	}
	if _, reuse := m.flags["--reuse"]; reuse {
		log, err := m.store.LoadLedger(m.change.ID)
		if err != nil {
			return result{}, err
		}
		for _, r := range slices.Backward(log) {
			if r.Closing == nil {
				continue
			}
			if r.Closing.Automatic && r.Closing.Command == closing.Command && r.Closing.State(before) == "pass" {
				return data(map[string]any{"change": m.change.ID, "ok": true, "closing": "pass", "record": r.Closing, "reused": true})
			}
			break
		}
	}
	closing.Before = before
	var output methodOutput
	if closing.Automatic {
		closing.Exit, err = m.runClosing(closing.Command, limit, &output)
		if err != nil {
			return result{}, err
		}
	}
	after, snapshotErr := method.Snapshot(m.ctx, m.root.Name())
	closing.After = after
	if err := m.store.RecordClosing(m.change.ID, m.now, closing); err != nil {
		return result{}, errors.Join(err, snapshotErr)
	}
	if snapshotErr != nil {
		return result{}, fmt.Errorf("已记录未验证的收尾，读取结束快照：%w", snapshotErr)
	}
	state := closing.State(after)
	return data(map[string]any{"change": m.change.ID, "ok": state == "pass", "closing": state, "record": closing, "reused": false, "output": strings.ToValidUTF8(string(output), "�")})
}

// 只保留诊断末尾，长测试日志不得占满内存或破坏错误信封。
type methodOutput []byte

// Write 保留命令的最近输出，包括以非零状态退出前的诊断。
func (tail *methodOutput) Write(p []byte) (int, error) {
	const limit = 2048
	n := len(p)
	if len(p) >= limit {
		*tail = append((*tail)[:0], p[n-limit:]...)
		return n, nil
	}
	if excess := len(*tail) + n - limit; excess > 0 {
		*tail = (*tail)[excess:]
	}
	*tail = append(*tail, p...)
	return n, nil
}

func (m *methodCLI) runClosing(command string, limit time.Duration, output io.Writer) (*int, error) {
	ctx, cancel := context.WithTimeout(m.ctx, limit)
	defer cancel()
	cmd := methodShell(ctx, command)
	cmd.Dir = m.root.Name()
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, nil
	}
	if err != nil {
		if _, ok := errors.AsType[*exec.ExitError](err); !ok {
			return nil, fmt.Errorf("运行收尾命令：%w", err)
		}
	}
	code := cmd.ProcessState.ExitCode()
	if code < 0 {
		return nil, nil
	}
	return &code, nil
}

func (m *methodCLI) verify() (result, error) {
	var report method.VerifierReport
	if err := readMethodJSON(m.flags["--report"], &report); err != nil {
		return result{}, err
	}
	by := m.flags["--by"]
	if report.By != "" && report.By != by {
		return result{}, invalid("验证者与报告署名不一致")
	}
	report.By = by
	req := m.requirement()
	if err := report.Check(req); err != nil {
		return result{}, invalid("报告：%v", err)
	}
	current, err := method.Snapshot(m.ctx, m.root.Name())
	if err != nil {
		return result{}, err
	}
	if err := m.store.RecordReview(m.change.ID, m.now, current, req, report); err != nil {
		if errors.Is(err, method.ErrClosingRequired) {
			return result{}, methodRefused(err)
		}
		return result{}, err
	}
	return data(map[string]any{"change": m.change.ID, "result": report.Result(), "report": report})
}

func (m *methodCLI) currentEvidence() (string, error) {
	// 尚无收尾记录时没有可比的快照，空仓库也能读取契约与事实。
	if !slices.ContainsFunc(m.log, func(r method.Record) bool { return r.Closing != nil }) {
		return "", nil
	}
	return method.Snapshot(m.ctx, m.root.Name())
}

func (m *methodCLI) evidence() (result, error) {
	current, err := m.currentEvidence()
	if err != nil {
		return result{}, err
	}
	c, err := m.store.Load(m.change.ID)
	if err != nil {
		return result{}, err
	}
	state := method.AssessEvidence(m.requirement(), m.log, current)
	return data(map[string]any{"change": m.change.ID, "confirmation": method.Verify(c, m.documents()...), "closing": state.Closing, "review": state.Review, "records": m.log})
}

func (m *methodCLI) note() (result, error) {
	current, err := m.currentEvidence()
	if err != nil {
		return result{}, err
	}
	text := method.ProjectChange(m.change.docs, m.log, current)
	payload := map[string]string{"change": m.change.ID, "text": text}
	if out := m.flags["--out"]; out != "" {
		if err := os.WriteFile(out, []byte(text), 0644); err != nil {
			return result{}, fmt.Errorf("写变更说明 %q：%w", out, err)
		}
		payload["path"] = out
	}
	return data(payload)
}
