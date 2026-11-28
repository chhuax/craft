package claudecode

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/chhuax/craft/internal/host"
)

var toolName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

func permissionArgs(p host.Permissions) ([]string, error) {
	// dontAsk 不提供网络或写目录隔离。Read 工具集合可用于只读请求；开放 Bash 就不能承诺只读。
	if p.NoNetwork || p.WriteScope != host.ReadOnly && p.WriteScope != "" {
		return nil, fmt.Errorf("%w: Claude Code 无法保证所需的目录或网络限制", host.Unsupported)
	}
	tools := p.Tools
	if p.WriteScope == host.ReadOnly && tools == nil {
		tools = []string{"Read"}
	}
	for _, name := range tools {
		if !toolName.MatchString(name) || p.WriteScope == host.ReadOnly && name != "Read" {
			return nil, fmt.Errorf("%w: 工具不能满足权限上限", host.Unsupported)
		}
	}
	if tools == nil {
		return nil, nil
	}
	return []string{"--permission-mode", "dontAsk", "--tools", strings.Join(tools, ","), "--strict-mcp-config"}, nil
}
