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
	for _, name := range []string{"craft", "craft-worker"} {
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
