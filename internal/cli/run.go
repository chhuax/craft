package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/chhuax/craft/internal/app"
)

const usage = `用法
  craft version [--json]    显示构建版本
  craft doctor [--json]     运行安装自检
  craft --help [--json]     显示帮助`

// Run 返回进程退出码，使宿主和终端能按同一契约区分参数错误与输出错误。
func Run(args []string, version string, stdout, stderr io.Writer) int {
	return run(args, version, stdout, stderr, os.Getenv, os.UserHomeDir)
}

func run(args []string, version string, stdout, stderr io.Writer, getenv func(string) string, userHomeDir func() (string, error)) int {
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
	case len(args) == 1 && args[0] == "doctor":
		return reportChecks(stdout, asJSON, app.Doctor(context.Background(), getenv, userHomeDir))
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
		err = writeEnvelope(stdout, true, map[string]string{key: value})
	} else {
		_, err = fmt.Fprintln(stdout, value)
	}
	if err != nil {
		return 1
	}
	return 0
}

func reportChecks(stdout io.Writer, asJSON bool, checks []app.Check) int {
	ok := !slices.ContainsFunc(checks, func(c app.Check) bool { return c.Status == "failed" })
	if asJSON {
		if err := writeEnvelope(stdout, ok, map[string][]app.Check{"checks": checks}); err != nil {
			return 1
		}
	} else {
		for _, c := range checks {
			// 路径和系统错误可能包含换行，引用说明可让每项始终只占一行。
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%q\n", c.Name, c.Status, c.Detail); err != nil {
				return 1
			}
		}
	}
	if !ok {
		return 1
	}
	return 0
}

func writeEnvelope(w io.Writer, ok bool, payload any) error {
	return json.NewEncoder(w).Encode(map[string]any{
		"ok": ok, "payload": payload, "evidence": []string{},
	})
}
