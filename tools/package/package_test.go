package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const testVersion = "1.2.3"

func packageEnv(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	t.Setenv("CRAFT_PACKAGE_FIXTURE", "1")
	t.Setenv("FIXTURE_VERSION", testVersion)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	return bin
}

func writeTestExe(path string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(exe)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0755)
}

func fakeBuild(root, path, version, target string) error {
	if target == runtime.GOOS+"/"+runtime.GOARCH {
		return writeTestExe(path)
	}
	return os.WriteFile(path, []byte(target+" "+version), 0600)
}

func makeTestPackage(t *testing.T) (string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plugin")
	var report bytes.Buffer
	if err := assemble("../..", out, testVersion, allTargets, fakeBuild, &report); err != nil {
		t.Fatal(err)
	}
	return out, report.String()
}

func TestUniversalPackage(t *testing.T) {
	packageEnv(t)
	first, report := makeTestPackage(t)
	second, _ := makeTestPackage(t)
	files := strings.Fields(`.claude-plugin/plugin.json .codex-plugin/plugin.json LICENSE NOTICE README.md VERSION bin/craft bin/craft.exe bin/darwin-amd64/craft bin/darwin-arm64/craft bin/linux-amd64/craft bin/linux-arm64/craft hooks/hooks.json hooks/codex-hooks.json skills/craft/SKILL.md skills/craft-worker/SKILL.md`)
	slices.Sort(files)
	for _, key := range append(slices.Clone(allTargets), "总量", "已跳过", "claude plugin validate") {
		if !strings.Contains(report, key) {
			t.Errorf("体积或校验报告缺少 %q: %s", key, report)
		}
	}
	type entry struct {
		mode   fs.FileMode
		at     time.Time
		digest [32]byte
	}
	snapshot := func(root string) map[string]entry {
		items := map[string]entry{}
		var actual []string
		err := fs.WalkDir(os.DirFS(root), ".", func(name string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			item := entry{mode: info.Mode(), at: info.ModTime()}
			if !d.IsDir() {
				actual = append(actual, name)
				raw, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					return err
				}
				item.digest = sha256.Sum256(raw)
				want := fs.FileMode(0644)
				if strings.HasPrefix(name, "bin/") {
					want = 0755
				}
				if runtime.GOOS != "windows" && item.mode.Perm() != want {
					t.Errorf("%s 模式 %v", name, item.mode)
				}
			}
			items[name] = item
			return nil
		})
		slices.Sort(actual)
		if err != nil || !slices.Equal(actual, files) {
			t.Fatalf("文件布局 %v: %v", actual, err)
		}
		return items
	}
	if !reflect.DeepEqual(snapshot(first), snapshot(second)) {
		t.Fatal("两次组装的内容、模式或修改时间不同")
	}
}

func TestPackageValidation(t *testing.T) {
	packageEnv(t)
	out, _ := makeTestPackage(t)
	for _, tc := range []struct{ name, path, from, to string }{
		{"文件缺失", "skills/craft/SKILL.md", "", ""},
		{"清单语法", ".claude-plugin/plugin.json", "{", "{!"},
		{"hook语法", "hooks/hooks.json", "{", "["},
		{"skill引用", ".codex-plugin/plugin.json", "./skills/", "./missing/"},
		{"hook引用", ".codex-plugin/plugin.json", "./hooks/codex-hooks.json", "./hooks/missing.json"},
		{"入口引用", "hooks/hooks.json", "/bin/craft", "/bin/missing"},
		{"Windows入口引用", "hooks/codex-hooks.json", "craft.exe", "missing.exe"},
		{"清单版本", ".codex-plugin/plugin.json", testVersion, "2.0"},
		{"包版本", "VERSION", testVersion, "1.0.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(out, tc.path)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.WriteFile(path, raw, 0644); err != nil {
					t.Error(err)
				}
			})
			if tc.from == "" {
				err = os.Remove(path)
			} else {
				changed := bytes.Replace(raw, []byte(tc.from), []byte(tc.to), 1)
				if bytes.Equal(changed, raw) {
					t.Fatal("样本未改变")
				}
				err = os.WriteFile(path, changed, 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := validate(out, testVersion, allTargets, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), tc.path) {
				t.Fatalf("未定位失败文件 %s: %v", tc.path, err)
			}
		})
	}
	t.Setenv("FIXTURE_VERSION", "other")
	if err := validate(out, testVersion, allTargets, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("未发现本机二进制版本错误: %v", err)
	}
}

func TestInvalidBinaryAndSizeReport(t *testing.T) {
	packageEnv(t)
	out, _ := makeTestPackage(t)
	native := runtime.GOOS + "/" + runtime.GOARCH
	if !slices.Contains(allTargets, native) {
		t.Skip("当前平台不在打包目标内")
	}
	path := filepath.Join(out, binaryPath(native))
	if runtime.GOOS != "windows" {
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if err := validate(out, testVersion, allTargets, &bytes.Buffer{}); err == nil {
			t.Fatal("未发现可执行位丢失")
		}
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("损坏的二进制"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validate(out, testVersion, allTargets, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatal(err)
	}

	var report strings.Builder
	reportSize(&report, native, 17<<20, 16<<20)
	if !strings.Contains(report.String(), "警告") {
		t.Fatal(report.String())
	}
}

func TestPackageHostValidation(t *testing.T) {
	bin := packageEnv(t)
	name := "claude"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := writeTestExe(filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
	out, report := makeTestPackage(t)
	if !strings.Contains(report, "模拟清单验证") {
		t.Fatal(report)
	}
	t.Setenv("FIXTURE_VALIDATE_FAIL", "1")
	if err := validate(out, testVersion, allTargets, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "claude plugin validate") {
		t.Fatalf("未报告宿主校验失败: %v", err)
	}
}

func TestPackageBuildFailure(t *testing.T) {
	packageEnv(t)
	out, _ := makeTestPackage(t)
	fail := func(_, _, _, target string) error { return fmt.Errorf("模拟编译失败 %s", target) }
	if err := assemble("../..", out, "2.0", allTargets, fail, &bytes.Buffer{}); err == nil {
		t.Fatal("未报告编译失败")
	}
	raw, err := os.ReadFile(filepath.Join(out, "VERSION"))
	if err != nil || strings.TrimSpace(string(raw)) != testVersion {
		t.Fatal("破坏了已验证的输出", err)
	}
}

func TestSelectedTarget(t *testing.T) {
	packageEnv(t)
	out := filepath.Join(t.TempDir(), "selected")
	var report bytes.Buffer
	if err := assemble("../..", out, testVersion, []string{"windows/amd64"}, fakeBuild, &report); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(out, "bin"))
	if err != nil || len(entries) != 2 || entries[0].Name() != "craft" || entries[1].Name() != "craft.exe" {
		t.Fatalf("裁剪构建入口 %v: %v", entries, err)
	}
	if runtime.GOOS != "windows" && !strings.Contains(report.String(), "本机 craft version 已跳过") {
		t.Fatal(report.String())
	}
}
