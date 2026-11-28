package method

import (
	"crypto/rand"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/template"
)

//go:embed templates/contract.md
var contractTemplate string

// Scaffold 在调用方指定的新目录生成文档。先在同一父目录写齐模板，再整体改名发布。
func Scaffold(dir, kind, tier string) (err error) {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("创建文档父目录：%w", err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return fmt.Errorf("打开文档父目录：%w", err)
	}
	defer closeResource(root, &err)
	return ScaffoldAt(root, filepath.Base(dir), kind, tier, "", nil, nil)
}

// ScaffoldAt 在项目根内生成带基线和关联信息的契约，临时文件与目标目录位于同一文件系统。
func ScaffoldAt(root *os.Root, dir, kind, tier, base string, items, capabilities []string) (err error) {
	if !validType(kind) || !validTier(tier) {
		return fmt.Errorf("%w：未知类型 %q 或档位 %q", fs.ErrInvalid, kind, tier)
	}
	if root == nil || !fs.ValidPath(dir) || !safePath(dir) {
		return fmt.Errorf("%w：变更目录 %q", fs.ErrInvalid, dir)
	}
	dir = filepath.FromSlash(dir)
	if _, err := root.Lstat(dir); err == nil {
		return fmt.Errorf("%w：变更目录 %q 已存在", fs.ErrExist, dir)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查变更目录：%w", err)
	}
	parent := filepath.Dir(dir)
	if err := root.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("创建文档父目录：%w", err)
	}
	staging := filepath.Join(parent, ".method-"+rand.Text())
	if err := root.Mkdir(staging, 0700); err != nil {
		return fmt.Errorf("暂存契约：%w", err)
	}
	defer func() { err = errors.Join(err, root.RemoveAll(staging)) }()
	if err := writeSkeletons(root, staging, kind, tier, base, items, capabilities); err != nil {
		return err
	}
	if err := root.Rename(staging, dir); err != nil {
		return fmt.Errorf("发布变更目录 %q：%w", dir, err)
	}
	return nil
}

func writeSkeletons(root *os.Root, staging, kind, tier, base string, items, capabilities []string) error {
	// 引号与反斜线按文件头解析器的规则转义，避免项目名称被当作标量或新的一行。
	quote := func(s string) (string, error) {
		if strings.ContainsFunc(s, func(r rune) bool { return r < 32 }) {
			return "", fmt.Errorf("%w：文件头含控制字符", fs.ErrInvalid)
		}
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`, nil
	}
	tpl, err := template.New("契约").Funcs(template.FuncMap{"quote": quote}).Parse(contractTemplate)
	if err != nil {
		return fmt.Errorf("解析契约模板：%w", err)
	}
	req, design := requiredSections(kind, tier)
	if kind != "explore" && tier != "light" {
		i := slices.Index(design, "验证")
		design = slices.Insert(design, i, "接口与库表变更")
	}
	write := func(name, title string, sections []string) (err error) {
		f, err := root.Create(filepath.Join(staging, name))
		if err != nil {
			return fmt.Errorf("创建模板 %s：%w", name, err)
		}
		defer closeResource(f, &err)
		args := struct {
			Version                 int
			Main                    bool
			Kind, Tier, Title       string
			Sections                []string
			BaseBranch              string
			WorkItems, Capabilities []string
		}{formatVersion, name != designFile, kind, tier, title, sections, base, items, capabilities}
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
