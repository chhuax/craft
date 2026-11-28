package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/app"
)

func invoke(t *testing.T, home string, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, "dev", &stdout, &stderr, func(key string) string {
		if key == "CRAFT_HOME" {
			return home
		}
		return ""
	}, func() (string, error) {
		t.Fatal("已有 CRAFT_HOME，不应读取用户目录")
		return "", nil
	})
	if stderr.Len() != 0 {
		t.Errorf("意外的标准错误输出：%s", &stderr)
	}
	return code, stdout.String()
}

func doctorJSON(t *testing.T, home string, wantCode int) map[string]app.Check {
	t.Helper()
	code, output := invoke(t, home, "doctor", "--json")
	var report struct {
		OK       *bool
		Payload  struct{ Checks []app.Check }
		Evidence []json.RawMessage
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		t.Fatal(err)
	}
	if code != wantCode || report.OK == nil || *report.OK != (wantCode == 0) ||
		report.Evidence == nil || len(report.Evidence) != 0 {
		t.Fatalf("退出码 = %d，报告 = %s", code, output)
	}
	checks := make(map[string]app.Check)
	for _, c := range report.Payload.Checks {
		checks[c.Name] = c
	}
	if len(checks) != 5 || len(report.Payload.Checks) != 5 {
		t.Fatalf("期望五个不同的检查项，实际为 %s", output)
	}
	return checks
}

func TestDoctor(t *testing.T) {
	home := t.TempDir()
	checks := doctorJSON(t, home, 0)
	for _, name := range []string{"stateDir", "databaseOpen", "databaseInfo", "extensions"} {
		if c := checks[name]; c.Status != "ok" || c.Detail == "" {
			t.Errorf("%s 检查 = %+v", name, c)
		}
	}
	for _, name := range []string{"host"} {
		if c := checks[name]; c.Status != "skipped" || !strings.Contains(c.Detail, "未实现") {
			t.Errorf("%s 检查 = %+v", name, c)
		}
	}
	detail := checks["databaseInfo"].Detail
	for _, value := range []string{"schema=1", "journal_mode=wal", "SQLite=3."} {
		if !strings.Contains(detail, value) {
			t.Errorf("库信息 %q 缺少 %q", detail, value)
		}
	}
	code, output := invoke(t, home, "doctor")
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if code != 0 || len(lines) != len(checks) {
		t.Fatalf("人读报告退出码 = %d，输出 = %s", code, output)
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != checks[fields[0]].Status {
			t.Fatalf("人读检查项缺少名称、状态或说明：%q", line)
		}
		delete(checks, fields[0])
	}
	if _, err := os.Stat(filepath.Join(home, "craft.db")); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), ".craft-doctor-") {
			t.Errorf("自检残留临时文件 %q", file.Name())
		}
	}
}

func TestDoctorUnwritableStateDir(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("当前平台或用户不能用目录权限位模拟禁止写入")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(home, 0o700); err != nil {
			t.Error(err)
		}
	})
	checks := doctorJSON(t, home, 1)
	if c := checks["stateDir"]; c.Status != "failed" || c.Detail == "" {
		t.Errorf("只读目录检查 = %+v", c)
	}
}

func TestDoctorNewerSchema(t *testing.T) {
	home := t.TempDir()
	// 样本由 PRAGMA page_size=512 和 PRAGMA user_version=999 生成，
	// 复制到临时目录可保留真实库格式，并让 CLI 测试无需 SQL 驱动或外部命令。
	data, err := os.ReadFile("schema999.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "craft.db"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	checks := doctorJSON(t, home, 1)
	if c := checks["databaseOpen"]; c.Status != "failed" ||
		!strings.Contains(c.Detail, "999") || !strings.Contains(c.Detail, "支持") {
		t.Errorf("高版本库的打开结果 = %+v", c)
	}
	if c := checks["databaseInfo"]; c.Status != "skipped" || c.Detail == "" {
		t.Errorf("未打开的库信息 = %+v", c)
	}
}

func TestHelpListsDoctor(t *testing.T) {
	code, output := invoke(t, t.TempDir(), "--help")
	if code != 0 || !strings.Contains(output, "craft doctor") {
		t.Errorf("帮助退出码 = %d，输出 = %s", code, output)
	}
}

func TestDoctorRejectedExtension(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "extensions", "broken")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	c := doctorJSON(t, home, 1)["extensions"]
	if c.Status != "failed" || !strings.Contains(c.Detail, "broken") {
		t.Fatal(c)
	}
}
