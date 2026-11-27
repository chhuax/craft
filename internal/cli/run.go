package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

const usage = `用法
  craft version [--json]    显示构建版本
  craft --help [--json]     显示帮助`

// Run 返回进程退出码，使宿主和终端能按同一契约区分参数错误与输出错误。
func Run(args []string, version string, stdout, stderr io.Writer) int {
	asJSON := slices.Contains(args, "--json")
	if asJSON {
		args = slices.DeleteFunc(slices.Clone(args), func(s string) bool { return s == "--json" })
	}

	var key, value string
	switch {
	case len(args) == 1 && args[0] == "version":
		key, value = "version", version
	case len(args) == 1 && args[0] == "--help":
		key, value = "help", usage
	default:
		message := "参数错误，请运行 craft --help 查看用法"
		if asJSON {
			err := json.NewEncoder(stderr).Encode(map[string]any{
				"ok":    false,
				"error": map[string]any{"code": "invalidArguments", "message": message, "detail": map[string]any{}},
			})
			if err != nil {
				return 1
			}
		} else if _, err := fmt.Fprintln(stderr, message); err != nil {
			return 1
		}
		return 2
	}

	var err error
	if asJSON {
		err = json.NewEncoder(stdout).Encode(map[string]any{
			"ok": true, "payload": map[string]string{key: value}, "evidence": []string{},
		})
	} else {
		_, err = fmt.Fprintln(stdout, value)
	}
	if err != nil {
		return 1
	}
	return 0
}
