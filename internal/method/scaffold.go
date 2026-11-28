package method

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"text/template"
)

//go:embed templates/contract.md
var contractTemplate string

// Scaffold 在调用方指定的新目录生成文档。先在同一父目录写齐模板，再整体改名发布。
func Scaffold(dir, kind, tier string) (err error) {
	if !validType(kind) || !validTier(tier) {
		return fmt.Errorf("生成文档：未知的类型 %q 或档位 %q", kind, tier)
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("生成文档 %q：目录已存在", dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查文档目录 %q：%w", dir, err)
	}
	tpl, err := template.New("契约").Parse(contractTemplate)
	if err != nil {
		return fmt.Errorf("解析文档模板：%w", err)
	}
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("创建文档父目录：%w", err)
	}
	staging, err := os.MkdirTemp(parent, ".method-")
	if err != nil {
		return fmt.Errorf("创建文档暂存目录：%w", err)
	}
	defer func() {
		if e := os.RemoveAll(staging); e != nil {
			err = errors.Join(err, fmt.Errorf("清理文档暂存目录：%w", e))
		}
	}()
	if err := writeSkeletons(staging, kind, tier, tpl); err != nil {
		return err
	}
	if err := os.Rename(staging, dir); err != nil {
		return fmt.Errorf("发布文档目录 %q：%w", dir, err)
	}
	return nil
}

func writeSkeletons(staging, kind, tier string, tpl *template.Template) error {
	req, design := requiredSections(kind, tier)
	if kind != "explore" && tier != "light" {
		i := slices.Index(design, "验证")
		design = slices.Insert(design, i, "接口与库表变更")
	}
	write := func(name, title string, sections []string) (err error) {
		f, err := os.Create(filepath.Join(staging, name))
		if err != nil {
			return fmt.Errorf("创建模板 %s：%w", name, err)
		}
		defer closeResource(f, &err)
		args := struct {
			Version           int
			Main              bool
			Kind, Tier, Title string
			Sections          []string
		}{formatVersion, name != designFile, kind, tier, title, sections}
		if err := tpl.Execute(f, args); err != nil {
			return fmt.Errorf("写入模板 %s：%w", name, err)
		}
		return nil
	}
	if tier == "light" {
		return write(combinedFile, "变更标题", append(append(req, design...), "假设与待确认"))
	}
	if err := write(requirementsFile, "需求标题", append(req, "假设与待确认")); err != nil {
		return err
	}
	return write(designFile, "方案标题", design)
}

func closeResource(c io.Closer, err *error) {
	if e := c.Close(); e != nil {
		*err = errors.Join(*err, fmt.Errorf("关闭文件或目录：%w", e))
	}
}
