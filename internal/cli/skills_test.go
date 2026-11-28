package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestSkillCommandsExist(t *testing.T) {
	// 扫描正文、行内示例和代码块，不要求命令出现在行首。
	command := regexp.MustCompile(`\bcraft[ \t]+([a-zA-Z0-9_-]+)(?:[ \t]+([a-zA-Z0-9_|-]+))?`)
	for _, name := range []string{"craft", "craft-worker", "method"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "hosts", "skills", name, "SKILL.md"))
		if err != nil {
			t.Fatal(err)
		}
		matches := command.FindAllStringSubmatch(string(raw), -1)
		if len(matches) == 0 || len(strings.Split(strings.TrimSpace(string(raw)), "\n")) > 150 {
			t.Errorf("%s 缺少命令或篇幅超限", name)
		}
		for _, match := range matches {
			for sub := range strings.SplitSeq(match[2], "|") {
				if _, _, err := findCommand([]string{match[1], sub}); err != nil {
					t.Errorf("%s 引用了未知命令 %q", name, match[0])
				}
			}
		}
	}
}

// 从插件安装目录也须读得到引用，正文里的方法命令须能在本地运行。
func TestMethodSkillReferences(t *testing.T) {
	root := filepath.Join("..", "..", "hosts", "skills", "method")
	link := regexp.MustCompile(`\]\(([^)]+)\)`)
	command := regexp.MustCompile(`\bcraft[ \t]+([a-z][a-z-]*(?: [a-z][a-z-]*){0,2})`)
	err := filepath.WalkDir(root, func(name string, e os.DirEntry, err error) error {
		if err != nil || e.IsDir() || filepath.Ext(name) != ".md" {
			return err
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		body := string(raw)
		for _, outside := range []string{"docs/", "http://", "https://"} {
			if strings.Contains(body, outside) {
				t.Errorf("%s 含外部引用 %q", name, outside)
			}
		}
		for _, m := range link.FindAllStringSubmatch(body, -1) {
			target := filepath.Join(filepath.Dir(name), filepath.FromSlash(m[1]))
			rel, err := filepath.Rel(root, target)
			if err != nil || !filepath.IsLocal(rel) {
				t.Errorf("%s 的引用越界 %s", name, m[1])
				continue
			}
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s 的引用：%v", name, err)
			}
		}
		for _, m := range command.FindAllStringSubmatch(body, -1) {
			spec, _, err := findCommand(strings.Fields(m[1]))
			if err != nil || spec.role != local || !strings.HasPrefix(spec.path, "method ") {
				t.Errorf("%s 的命令 %s", name, m[0])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
