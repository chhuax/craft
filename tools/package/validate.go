package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

func validate(root, version string, targets []string, out io.Writer) (err error) {
	var report strings.Builder
	defer func() {
		_, writeErr := io.WriteString(out, report.String())
		err = errors.Join(err, writeErr)
	}()
	raw, err := os.ReadFile(filepath.Join(root, "VERSION"))
	if err != nil {
		return fmt.Errorf("读取 VERSION: %w", err)
	}
	if string(raw) != version+"\n" {
		return fmt.Errorf("VERSION 内容 %q 与构建版本 %q 不同", raw, version)
	}
	sizes, err := inspectFiles(root, version, targets)
	if err != nil {
		return err
	}
	for _, target := range targets {
		reportSize(&report, target, sizes[binaryPath(target)], 16<<20)
	}
	var total int64
	for _, size := range sizes {
		total += size
	}
	reportSize(&report, "总量", total, 80<<20)
	native := runtime.GOOS + "/" + runtime.GOARCH
	if slices.Contains(targets, native) {
		if err := checkVersion(filepath.Join(root, binaryPath(native)), version); err != nil {
			return err
		}
		fmt.Fprintf(&report, "%s craft version = %s\n", native, version)
	} else {
		report.WriteString("本机 craft version 已跳过（未选择本机目标）\n")
	}
	return validateClaude(root, &report)
}

func inspectFiles(root, version string, targets []string) (map[string]int64, error) {
	sizes := make(map[string]int64)
	err := fs.WalkDir(os.DirFS(root), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm() != fileMode(name, false) {
			return fmt.Errorf("%s 文件类型或权限错误: %s", name, info.Mode())
		}
		sizes[name] = info.Size()
		if strings.HasSuffix(name, ".json") {
			raw, err := os.ReadFile(filepath.Join(root, name))
			if err != nil {
				return err
			}
			if err := checkJSON(root, name, version, raw); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, name := range packageFiles(targets) {
		if _, ok := sizes[name]; !ok {
			return nil, fmt.Errorf("缺少必需文件 %s", name)
		}
	}
	return sizes, nil
}

func checkVersion(path, version string) error {
	raw, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version: %w: %s", path, err, raw)
	}
	if strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r") != version {
		return fmt.Errorf("%s version 输出 %q，与 VERSION %q 不符", path, raw, version)
	}
	return nil
}

func reportSize(report *strings.Builder, name string, size, limit int64) {
	fmt.Fprintf(report, "%s %d 字节", name, size)
	if size > limit {
		fmt.Fprintf(report, "（警告：超过 %d MiB）", limit>>20)
	}
	fmt.Fprintln(report)
}

func checkJSON(root, name, version string, raw []byte) error {
	if !json.Valid(raw) {
		return errors.New("JSON 无效")
	}
	switch name {
	case claudeManifest, codexManifest:
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		if fields["version"] != version {
			return fmt.Errorf("清单版本 %v 与 VERSION %q 不同", fields["version"], version)
		}
		if name == codexManifest && (fields["hooks"] != "./hooks/codex-hooks.json" || fields["skills"] != "./skills/") {
			return errors.New("hooks 或 skills 未指向包内约定的路径")
		}
		return manifestPaths(root, fields)
	case "hooks/hooks.json", "hooks/codex-hooks.json":
		return checkHook(root, name, raw)
	}
	return nil
}

func manifestPaths(root string, v any) error {
	switch value := v.(type) {
	case map[string]any:
		for _, child := range value {
			if err := manifestPaths(root, child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range value {
			if err := manifestPaths(root, child); err != nil {
				return err
			}
		}
	case string:
		if strings.HasPrefix(value, "./") {
			p := strings.TrimSuffix(strings.TrimPrefix(value, "./"), "/")
			if !fs.ValidPath(p) || !filepath.IsLocal(p) {
				return fmt.Errorf("引用路径越出包根 %q", value)
			}
			if _, err := os.Stat(filepath.Join(root, p)); err != nil {
				return fmt.Errorf("引用 %q: %w", value, err)
			}
		}
	}
	return nil
}

func checkHook(root, name string, raw []byte) error {
	var config struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Type, Command, CommandWindows, StatusMessage string
				Timeout                                      int
			}
		}
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return err
	}
	if len(config.Hooks) != 2 {
		return errors.New("需要 SessionStart 和 Stop 两项钩子")
	}
	claude := name == "hooks/hooks.json"
	for _, event := range []struct{ name, command string }{{"SessionStart", "session-start"}, {"Stop", "stop"}} {
		groups := config.Hooks[event.name]
		if len(groups) != 1 || len(groups[0].Hooks) != 1 {
			return fmt.Errorf("需要一个 %s 命令", event.name)
		}
		h := groups[0].Hooks[0]
		if h.Type != "command" || h.Timeout != 15 || event.name == "Stop" && groups[0].Matcher != "" {
			return fmt.Errorf("%s 类型、超时或 matcher 错误", event.name)
		}
		// 按宿主约定核对入口，避免执行素材里的 shell 文本。
		if claude {
			if h.Command != `sh "${CLAUDE_PLUGIN_ROOT}/bin/craft" hook `+event.command+" --host claude-code" || h.CommandWindows != "" ||
				event.name == "SessionStart" && groups[0].Matcher != "startup|resume|clear|compact" {
				return fmt.Errorf("不符合 Claude Code 的 %s 钩子约定", event.name)
			}
		} else if h.Command != `sh "${PLUGIN_ROOT}/bin/craft" hook `+event.command+" --host codex" ||
			h.CommandWindows != `"${PLUGIN_ROOT}\bin\craft.exe" hook `+event.command+" --host codex" ||
			event.name == "SessionStart" && h.StatusMessage == "" {
			return fmt.Errorf("不符合 Codex 的 %s 钩子约定", event.name)
		}
	}
	if !claude {
		if _, err := os.Stat(filepath.Join(root, "bin/craft.exe")); err != nil {
			return fmt.Errorf("commandWindows 引用: %w", err)
		}
	}
	_, err := os.Stat(filepath.Join(root, "bin/craft"))
	return err
}

func validateClaude(root string, report *strings.Builder) (err error) {
	claude, err := exec.LookPath("claude")
	if errors.Is(err, exec.ErrNotFound) {
		_, err = fmt.Fprintln(report, "claude plugin validate 已跳过（PATH 中没有 claude）")
		return err
	}
	if err != nil {
		return fmt.Errorf("定位 claude: %w", err)
	}
	// 验证器只能在隔离目录中读写自己的宿主配置，不能用用户真实的配置或状态。
	dir, err := os.MkdirTemp(filepath.Dir(root), ".craft-validate-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	cmd := exec.Command(claude, "plugin", "validate", root)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+dir, "USERPROFILE="+dir, "CLAUDE_CONFIG_DIR="+dir,
		"XDG_CONFIG_HOME="+dir, "XDG_CACHE_HOME="+dir, "XDG_STATE_HOME="+dir, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	raw, err := cmd.CombinedOutput()
	fmt.Fprintf(report, "claude plugin validate\n%s\n", raw)
	if err != nil {
		return fmt.Errorf("claude plugin validate: %w", err)
	}
	report.WriteString("claude plugin validate 通过\n")
	return nil
}
