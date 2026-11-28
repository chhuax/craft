package cli

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/chhuax/craft/internal/method"
)

const methodInterfaces = "docs/interfaces"

// 已有文档沿用能力名前的序号；新能力无需额外的登记表。
var methodIndex = regexp.MustCompile("^[0-9]{2}-")

func (m *methodCLI) archive() (_ result, err error) {
	lock, err := m.lockDocuments()
	if err != nil {
		return result{}, err
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	base := map[method.InterfaceFile]string{}
	names := map[method.InterfaceFile]string{}
	entries, err := fs.ReadDir(m.root.FS(), methodInterfaces)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return result{}, fmt.Errorf("列出现状文档：%w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		capability, ok := strings.CutSuffix(e.Name(), "-api.md")
		kind := "api"
		if !ok {
			capability, ok = strings.CutSuffix(e.Name(), "-data.md")
			if !ok {
				continue
			}
			kind = "table"
		}

		name := path.Join(methodInterfaces, e.Name())
		raw, err := m.root.ReadFile(filepath.FromSlash(name))
		if err != nil {
			return result{}, fmt.Errorf("读取 %s：%w", name, err)
		}
		// 能力名本身也可以以数字开头；同一文件保留全名与去序号的查找键。
		for _, cap := range slices.Compact([]string{capability, methodIndex.ReplaceAllString(capability, "")}) {
			key := method.InterfaceFile{Capability: cap, Kind: kind}
			if other, exists := names[key]; exists {
				return result{}, invalid("现状文档重复：%s 与 %s", other, name)
			}
			names[key], base[key] = name, string(raw)
		}
	}
	merged, err := method.MergeInterfaces(base, m.design(), m.change.ID)
	if err != nil {
		return result{}, methodRefused(err)
	}
	updates := map[string]string{}
	for key, text := range merged {
		if old, ok := base[key]; ok && old == text {
			continue
		}
		name := names[key]
		if name == "" {
			suffix := "-api.md"
			if key.Kind == "table" {
				suffix = "-data.md"
			}
			name = path.Join(methodInterfaces, key.Capability+suffix)
		}
		if _, duplicate := updates[name]; duplicate {
			return result{}, invalid("多个增量指向同一文件 %s", name)
		}
		updates[name] = text
	}
	paths := slices.Sorted(maps.Keys(updates))
	for _, name := range paths {
		if err := m.replaceSpec(name, updates[name]); err != nil {
			return result{}, err
		}
	}
	return data(map[string]any{"change": m.change.ID, "files": paths})
}

func (m *methodCLI) replaceSpec(name, text string) (err error) {
	if err := m.root.MkdirAll(filepath.FromSlash(methodInterfaces), 0755); err != nil {
		return err
	}
	// MergeInterfaces 可重放。用同目录的完整文件替换，意外中断后重跑会补齐余项。
	tmp := filepath.Join(methodInterfaces, ".method-"+rand.Text())
	file, err := m.root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("暂存 %s：%w", name, err)
	}
	defer func() {
		if e := m.root.Remove(tmp); !errors.Is(e, fs.ErrNotExist) {
			err = errors.Join(err, e)
		}
	}()
	_, err = file.WriteString(text)
	if err = errors.Join(err, file.Close()); err != nil {
		return fmt.Errorf("写入 %s：%w", name, err)
	}
	return m.root.Rename(tmp, filepath.FromSlash(name))
}
