package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("CRAFT_PACKAGE_FIXTURE") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "uname":
			if os.Args[1] == "-s" {
				fmt.Println(os.Getenv("FIXTURE_SYSTEM"))
			} else {
				fmt.Println(os.Getenv("FIXTURE_MACHINE"))
			}
			os.Exit(0)
		case "craft", "craft.exe":
			if len(os.Args) > 1 && os.Args[1] == "version" {
				fmt.Println(os.Getenv("FIXTURE_VERSION"))
			} else {
				fmt.Printf("%s\n%q", filepath.ToSlash(os.Args[0]), os.Args[1:])
			}
			os.Exit(0)
		case "claude", "claude.exe":
			if len(os.Args) == 4 && os.Args[1] == "plugin" && os.Args[2] == "validate" &&
				os.Getenv("CLAUDE_CONFIG_DIR") != "" && os.Getenv("FIXTURE_VALIDATE_FAIL") == "" {
				fmt.Println("模拟清单验证")
				os.Exit(0)
			}
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func TestLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh 与可执行位在 Unix 上验证")
	}
	bin := filepath.Join(t.TempDir(), "bin with spaces")
	if err := os.Mkdir(bin, 0755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat("../../hosts/bin/craft")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0111 == 0 {
		t.Fatal("源码启动器缺少可执行位")
	}
	raw, err := os.ReadFile("../../hosts/bin/craft")
	if err != nil {
		t.Fatal(err)
	}
	launcher := filepath.Join(bin, "craft")
	if err := os.WriteFile(launcher, raw, 0755); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"uname", "darwin-arm64/craft", "darwin-amd64/craft", "linux-arm64/craft", "linux-amd64/craft", "craft.exe"} {
		path := filepath.Join(bin, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(exe, path); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CRAFT_PACKAGE_FIXTURE", "1")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, tc := range []struct{ system, arch, want string }{
		{"Darwin", "arm64", "darwin-arm64/craft"}, {"Darwin", "x86_64", "darwin-amd64/craft"},
		{"Linux", "aarch64", "linux-arm64/craft"}, {"Linux", "x86_64", "linux-amd64/craft"},
		{"MINGW64_NT-10.0", "x86_64", "craft.exe"}, {"MSYS_NT-10.0", "x86_64", "craft.exe"},
		{"FreeBSD", "x86_64", ""}, {"Linux", "riscv64", ""},
	} {
		t.Run(tc.system+"-"+tc.arch, func(t *testing.T) {
			t.Setenv("FIXTURE_SYSTEM", tc.system)
			t.Setenv("FIXTURE_MACHINE", tc.arch)
			out, err := exec.Command(launcher, "watch", "two words", "*").CombinedOutput()
			if tc.want == "" {
				if err == nil || len(out) == 0 {
					t.Fatalf("缺少平台错误: %v %s", err, out)
				}
				return
			}
			if want := filepath.ToSlash(filepath.Join(bin, tc.want)) + "\n" + `["watch" "two words" "*"]`; err != nil || string(out) != want {
				t.Fatalf("入口或参数错误: %v %s", err, out)
			}
		})
	}
	if err := os.Remove(filepath.Join(bin, "linux-arm64/craft")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXTURE_SYSTEM", "Linux")
	t.Setenv("FIXTURE_MACHINE", "arm64")
	if out, err := exec.Command(launcher).CombinedOutput(); err == nil || !strings.Contains(string(out), "linux-arm64") {
		t.Fatalf("缺少二进制诊断: %v %s", err, out)
	}
}
