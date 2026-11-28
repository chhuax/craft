package claudecode

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/host"
)

// 这些断言固化 2026-11-28 在 Claude Code 2.1.330 上的实测：
// 目录边界必须用带路径的 Edit(//<真实路径>/**) 规则（裸 Write 会放开全盘），
// Bash 只能经 Bash(<命令>:*) 前缀放行。
func TestWorkspacePermissionsUseScopedRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上的目录规则尚未验证，驱动明确返回 Unsupported")
	}
	// macOS 的临时目录经符号链接指向 /private/...，规则必须按真实路径写。
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	args, err := permissionArgs(host.Permissions{WriteScope: host.Workspace, Commands: []string{"craft"}}, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--permission-mode", "dontAsk", "--tools", "Read,Write,Edit,Glob,Grep,Bash", "--strict-mcp-config", "--allowedTools", "Edit(/" + dir + "/**)", "Bash(craft:*)"}
	if !slices.Equal(args, want) {
		t.Fatalf("参数 %q，期望 %q", args, want)
	}
	for _, a := range args {
		if a == "Write" || a == "Bash" || strings.HasPrefix(a, "Edit(") && !strings.Contains(a, "/**)") {
			t.Fatalf("出现了会放开全盘或开放 Bash 的裸规则: %q", a)
		}
	}
}

func TestWorkspaceRuleResolvesSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("符号链接与目录规则在 Windows 上未验证")
	}
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("无法创建符号链接:", err)
	}
	got, err := workspaceRule(link)
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	if got != "Edit(/"+resolved+"/**)" {
		t.Fatalf("规则 %q 没有使用解析后的真实路径 %q", got, resolved)
	}
}

func TestPermissionsRefuseWhatCannotBeGuaranteed(t *testing.T) {
	dir := t.TempDir()
	unsafeDir := filepath.Join(dir, "a(b)")
	if err := os.Mkdir(unsafeDir, 0o700); err != nil {
		t.Skip("无法创建含括号的目录:", err)
	}
	for name, tc := range map[string]struct {
		p   host.Permissions
		dir string
	}{
		"断网":                  {host.Permissions{NoNetwork: true}, dir},
		"worktree 范围":         {host.Permissions{WriteScope: host.Worktree}, dir},
		"只读加开放 Bash":          {host.Permissions{WriteScope: host.ReadOnly, Tools: []string{"Bash"}}, dir},
		"工作区加开放 Bash":         {host.Permissions{WriteScope: host.Workspace, Tools: []string{"Bash"}}, dir},
		"没有写范围却限定命令":          {host.Permissions{Commands: []string{"craft"}}, dir},
		"工作区缺目录":              {host.Permissions{WriteScope: host.Workspace}, ""},
		"工作区相对路径":             {host.Permissions{WriteScope: host.Workspace}, "relative/dir"},
		"工作区目录含规则语法字符":        {host.Permissions{WriteScope: host.Workspace}, unsafeDir},
		"命令名含空格":              {host.Permissions{WriteScope: host.Workspace, Commands: []string{"craft x"}}, dir},
		"命令名含分号":              {host.Permissions{WriteScope: host.Workspace, Commands: []string{"craft;rm"}}, dir},
		"命令名含括号":              {host.Permissions{WriteScope: host.Workspace, Commands: []string{"craft)"}}, dir},
		"工作区工具含 NotebookEdit": {host.Permissions{WriteScope: host.Workspace, Tools: []string{"NotebookEdit"}}, dir},
		// 工具名原样进命令行，不合法的名字就是参数注入：每一种写范围下都要拦住。
		"默认范围的工具名是选项":  {host.Permissions{Tools: []string{"--settings"}}, dir},
		"只读范围的工具名是选项":  {host.Permissions{WriteScope: host.ReadOnly, Tools: []string{"--settings"}}, dir},
		"工作区范围的工具名是选项": {host.Permissions{WriteScope: host.Workspace, Tools: []string{"--settings"}}, dir},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := permissionArgs(tc.p, tc.dir); !errors.Is(err, host.Unsupported) {
				t.Fatalf("应当返回 Unsupported，得到 %v", err)
			}
		})
	}
}

func TestReadOnlyKeepsOnlyReadWithOptionalCommands(t *testing.T) {
	args, err := permissionArgs(host.Permissions{WriteScope: host.ReadOnly, Commands: []string{"craft"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--permission-mode", "dontAsk", "--tools", "Read,Bash", "--strict-mcp-config", "--allowedTools", "Bash(craft:*)"}
	if !slices.Equal(args, want) {
		t.Fatalf("参数 %q，期望 %q", args, want)
	}
}
