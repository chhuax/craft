package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/app"
)

func TestExtTestOutput(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRAFT_HOME", root)
	for _, asJSON := range []bool{false, true} {
		args := []string{"ext", "test", filepath.Join(root, "missing"), "--fixtures", root}
		if asJSON {
			args = append(args, "--json")
		}
		var out, diag bytes.Buffer
		exit := run(args, "test", &out, &diag, func(string) string { return "" }, func() (string, error) { return "", errors.New("不应读取用户目录") })
		if exit != 1 || diag.Len() != 0 {
			t.Fatalf("退出 %d，诊断 %s", exit, &diag)
		}
		if asJSON {
			var env struct {
				OK      bool
				Payload struct{ Checks []app.Check }
				Error   struct{ Code string }
			}
			if err := json.Unmarshal(out.Bytes(), &env); err != nil || env.OK || len(env.Payload.Checks) < 11 || env.Payload.Checks[0].Status != "fail" || env.Error.Code == "" {
				t.Fatal(out.String(), err)
			}
		} else if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) < 11 || !strings.Contains(lines[0], "fail") {
			t.Fatal(out.String())
		}
	}
	command(t, root, 2, "ext", "test", root, "--fixtures")
	boundCommand(t, root, &app.SessionBinding{}, 4, "ext", "test", root)
}
