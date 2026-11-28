package conformance

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// 用发行入口核对冷启动和保留游标的热重放，确保 fixture 也能由命令行直接使用。
func TestGitHubExtension(t *testing.T) {
	src := filepath.Join("..", "..", "extensions", "github")
	dir := filepath.Join(t.TempDir(), "github")
	must(t, os.Mkdir(dir, 0700))
	t.Setenv("CRAFT_HOME", t.TempDir())
	t.Setenv("CRAFT_FIXTURE_GITHUB_TOKEN", "offline-replay")
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOSUMDB", "off")
	data, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	must(t, err)
	var m map[string]json.RawMessage
	must(t, json.Unmarshal(data, &m))
	var argv []string
	must(t, json.Unmarshal(m["exec"], &argv))
	if runtime.GOOS == "windows" {
		argv[0] += ".exe"
	}
	m["exec"], err = json.Marshal(argv)
	must(t, err)
	writeJSON(t, filepath.Join(dir, "manifest.json"), m)
	cmd := exec.CommandContext(t.Context(), "go", "-C", src, "build", "-o", filepath.Join(dir, argv[0]), ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建参考扩展: %v\n%s", err, output)
	}
	results := Run(t.Context(), dir, Options{Fixtures: filepath.Join(src, "testdata", "conformance")})
	checkStatuses(t, results, nil, nil)
}
