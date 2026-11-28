package claudecode

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/chhuax/craft/internal/host"
)

var (
	toolName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)
	// 命令名只允许字母、数字、点、下划线与连字符：规则语法里的括号、冒号、空格和通配符都不能出现。
	commandName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

// workspaceTools 是「工作区内可写」时开放的内置工具。Bash 不在其中：它只能经 Commands 以前缀规则放行。
var workspaceTools = map[string]bool{"Read": true, "Write": true, "Edit": true, "Glob": true, "Grep": true}

// permissionArgs 把中立的权限上限翻译成 Claude Code 的命令行参数。
//
// Claude Code 的 dontAsk 模式不是操作系统沙箱：它只在「工具集合」与「预批准规则」两层起作用。
// 2026-11-28 在 Claude Code 2.1.330 上实测得到下面三条，是这里所有写法的依据：
//   - 裸的 `--allowedTools Write` 会让 Write 能写到工作目录之外，不能当作目录边界；
//   - 带路径的 `Edit(//<绝对路径>/**)` 规则才把所有文件编辑类工具限定在该目录内（目录外被拒）；
//   - `Bash(craft:*)` 只放行单条 craft 命令，`ls`、`&&`、`;`、`$()` 都被拒。
//
// 因此只有「工作区内可写」与「只读」两种范围能兑现；Worktree、断网、开放 Bash 一律 Unsupported。
// Read 工具不受目录限制：它能读该用户能读的任何文件，这一点要在对外文档里写明。
func permissionArgs(p host.Permissions, dir string) ([]string, error) {
	if p.NoNetwork {
		return nil, fmt.Errorf("%w: Claude Code 无法保证断网", host.Unsupported)
	}
	tools, allow := p.Tools, []string(nil)
	switch p.WriteScope {
	case "":
		if len(p.Commands) > 0 {
			return nil, fmt.Errorf("%w: 限定命令必须同时指定写范围", host.Unsupported)
		}
		// 工具名会原样进命令行（--tools a,b），不合法的名字（例如 --settings）就是参数注入，必须在这里拦住。
		for _, name := range tools {
			if !toolName.MatchString(name) {
				return nil, fmt.Errorf("%w: 工具名 %q 不合法", host.Unsupported, name)
			}
		}
	case host.ReadOnly:
		if tools == nil {
			tools = []string{"Read"}
		}
		for _, name := range tools {
			if !toolName.MatchString(name) || name != "Read" {
				return nil, fmt.Errorf("%w: 工具不能满足只读上限", host.Unsupported)
			}
		}
	case host.Workspace:
		rule, err := workspaceRule(dir)
		if err != nil {
			return nil, err
		}
		if tools == nil {
			tools = []string{"Read", "Write", "Edit", "Glob", "Grep"}
		}
		for _, name := range tools {
			if !workspaceTools[name] {
				return nil, fmt.Errorf("%w: 工具 %q 不能满足工作区写范围", host.Unsupported, name)
			}
		}
		allow = append(allow, rule)
	default:
		return nil, fmt.Errorf("%w: Claude Code 无法保证 %q 范围", host.Unsupported, p.WriteScope)
	}
	for _, name := range p.Commands {
		if !commandName.MatchString(name) {
			return nil, fmt.Errorf("%w: 命令名 %q 不合法", host.Unsupported, name)
		}
		allow = append(allow, "Bash("+name+":*)")
	}
	if len(p.Commands) > 0 {
		tools = append(append([]string(nil), tools...), "Bash")
	}
	if tools == nil {
		return nil, nil
	}
	args := []string{"--permission-mode", "dontAsk", "--tools", strings.Join(tools, ","), "--strict-mcp-config"}
	if len(allow) > 0 {
		args = append(args, "--allowedTools")
		args = append(args, allow...)
	}
	return args, nil
}

// workspaceRule 生成把文件编辑限定在 dir 内的预批准规则。必须先解析符号链接：macOS 上 /tmp 实际是
// /private/tmp，规则按真实路径匹配，不解析的话写目录内的文件也会被拒。
func workspaceRule(dir string) (string, error) {
	if runtime.GOOS == "windows" {
		// Windows 上路径规则的写法（盘符、反斜杠）没有验证过，不假装支持。
		return "", fmt.Errorf("%w: Windows 上的目录规则尚未验证", host.Unsupported)
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%w: 工作区范围需要绝对路径的工作目录", host.Unsupported)
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(dir))
	if err != nil {
		return "", fmt.Errorf("%w: 解析工作目录: %v", host.Unsupported, err)
	}
	if strings.ContainsAny(real, "()*?[]{},!\\\n\r") {
		return "", fmt.Errorf("%w: 工作目录路径含有会破坏权限规则语法的字符", host.Unsupported)
	}
	return "Edit(/" + real + "/**)", nil
}
