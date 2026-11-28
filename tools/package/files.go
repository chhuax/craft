package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	claudeManifest = ".claude-plugin/plugin.json"
	codexManifest  = ".codex-plugin/plugin.json"
)

var assets = []string{
	claudeManifest, codexManifest, "LICENSE", "NOTICE", "README.md", "bin/craft",
	"hooks/codex-hooks.json", "hooks/hooks.json", "skills/craft-worker/SKILL.md", "skills/craft/SKILL.md",
	"skills/method/SKILL.md",
	"skills/method/assets/change.md",
	"skills/method/assets/design.md",
	"skills/method/assets/requirements.md",
	"skills/method/examples/light-defect/change.md",
	"skills/method/examples/standard-feature/design.md",
	"skills/method/examples/standard-feature/requirements.md",
	"skills/method/examples/standard-feature/tasks.md",
	"skills/method/references/implement.md",
	"skills/method/references/intent.md",
	"skills/method/references/interfaces.md",
	"skills/method/references/tasks.md",
	"skills/method/references/team.md",
	"skills/method/references/tiers.md",
	"skills/method/references/types.md",
	"skills/method/references/verify.md",
	"skills/method/references/writing.md",
}

func sourceFile(name string) string {
	switch name {
	case claudeManifest:
		return "hosts/claude-code/" + name
	case codexManifest:
		return "hosts/codex/" + name
	case "LICENSE", "NOTICE":
		return name
	default:
		return "hosts/" + name
	}
}

func packageFiles(targets []string) []string {
	names := append(slices.Clone(assets), "VERSION")
	for _, target := range targets {
		names = append(names, binaryPath(target))
	}
	slices.Sort(names)
	return names
}

func populate(root, stage, version string, targets []string, build buildBinary) error {
	for _, name := range assets {
		raw, err := os.ReadFile(filepath.Join(root, sourceFile(name)))
		if err != nil {
			return fmt.Errorf("读取包素材 %s: %w", name, err)
		}
		if name == claudeManifest || name == codexManifest {
			raw, err = versionManifest(raw, version)
			if err != nil {
				return fmt.Errorf("写入 %s 版本: %w", name, err)
			}
		}
		path := filepath.Join(stage, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("准备 %s: %w", name, err)
		}
		if err := os.WriteFile(path, raw, 0644); err != nil {
			return fmt.Errorf("写入 %s: %w", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(stage, "VERSION"), []byte(version+"\n"), 0644); err != nil {
		return fmt.Errorf("写入 VERSION: %w", err)
	}
	for _, target := range targets {
		path := filepath.Join(stage, binaryPath(target))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return fmt.Errorf("准备 %s: %w", target, err)
		}
		if err := build(root, path, version, target); err != nil {
			return fmt.Errorf("构建 %s: %w", target, err)
		}
	}
	return normalize(stage)
}

func versionManifest(raw []byte, version string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, errors.New("清单必须是 JSON 对象")
	}
	value, err := json.Marshal(version)
	if err != nil {
		return nil, err
	}
	fields["version"] = value
	out, err := json.MarshalIndent(fields, "", "  ")
	return append(out, '\n'), err
}

func fileMode(name string, dir bool) fs.FileMode {
	if dir || strings.HasPrefix(name, "bin/") {
		return 0755
	}
	return 0644
}

func normalize(root string) error {
	stamp := time.Unix(946684800, 0).UTC()
	// 等编译和复制全部结束再设目录时间，避免创建子项改变父目录的修改时间。
	return fs.WalkDir(os.DirFS(root), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		path := filepath.Join(root, name)
		if err := os.Chmod(path, fileMode(name, d.IsDir())); err != nil {
			return fmt.Errorf("设置 %s 模式: %w", name, err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			return fmt.Errorf("设置 %s 时间: %w", name, err)
		}
		return nil
	})
}

// 只替换空目录或本工具的上一份包，防止 --out 误指向含其他文件的目录。
func checkDestination(out string) error {
	entries, err := os.ReadDir(out)
	if err != nil {
		return fmt.Errorf("检查输出目录: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(out, codexManifest)); err != nil {
		return fmt.Errorf("已有目录不是 craft 包: %w", err)
	}
	files := packageFiles(allTargets)
	return fs.WalkDir(os.DirFS(out), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || name == "." {
			return err
		}
		if d.IsDir() && slices.ContainsFunc(files, func(f string) bool { return strings.HasPrefix(f, name+"/") }) ||
			d.Type().IsRegular() && slices.Contains(files, name) {
			return nil
		}
		return fmt.Errorf("输出目录含包以外的文件 %q", name)
	})
}

func publish(stage, out string) error {
	if _, err := os.Lstat(out); errors.Is(err, os.ErrNotExist) {
		return os.Rename(stage, out)
	}
	if err := checkDestination(out); err != nil {
		return fmt.Errorf("检查输出 %s: %w", out, err)
	}
	// 旧包在新包通过校验后才移开。Windows 也允许将目录改名到不存在的路径。
	previous := stage + ".old"
	if err := os.Rename(out, previous); err != nil {
		return fmt.Errorf("保留旧包: %w", err)
	}
	if err := os.Rename(stage, out); err != nil {
		return errors.Join(fmt.Errorf("写入新包: %w", err), os.Rename(previous, out))
	}
	return os.RemoveAll(previous)
}
