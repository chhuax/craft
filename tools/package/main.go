// 组装两个宿主共用的发布目录，并在交付前核对内容与本机入口。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

var allTargets = []string{"darwin/amd64", "darwin/arm64", "linux/amd64", "linux/arm64", "windows/amd64"}

func main() {
	version := flag.String("version", "", "写入清单与二进制的版本")
	out := flag.String("out", "", "插件包输出目录")
	list := flag.String("targets", strings.Join(allTargets, ","), "逗号分隔的系统/架构")
	flag.Parse()
	targets := strings.Split(*list, ",")
	err := checkArgs(*version, *out, targets)
	if err == nil && flag.NArg() != 0 {
		err = errors.New("存在多余的位置参数")
	}
	if err == nil {
		slices.Sort(targets)
		err = assemble(".", *out, *version, targets, goBuild, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func checkArgs(version, out string, targets []string) error {
	if version == "" || strings.ContainsAny(version, " \t\r\n\x00") || out == "" {
		return errors.New("需要 --version V（不含空白）和 --out DIR")
	}
	seen := map[string]bool{}
	for _, target := range targets {
		if !slices.Contains(allTargets, target) || seen[target] {
			return fmt.Errorf("不支持或重复的目标 %q，可选 %s", target, strings.Join(allTargets, ", "))
		}
		seen[target] = true
	}
	return nil
}

type buildBinary func(root, path, version, target string) error

func goBuild(root, path, version, target string) error {
	system, arch, _ := strings.Cut(target, "/")
	// 版本已显式注入，不让工作树的脏标记、提交时间或临时输出路径改变二进制。
	cmd := exec.Command("go", "build", "-buildvcs=false", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", path, "./cmd/craft")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+system, "GOARCH="+arch)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build: %w\n%s", err, raw)
	}
	return nil
}

func binaryPath(target string) string {
	if target == "windows/amd64" {
		return "bin/craft.exe"
	}
	return "bin/" + strings.ReplaceAll(target, "/", "-") + "/craft"
}

func assemble(root, out, version string, targets []string, build buildBinary, report io.Writer) (err error) {
	out, err = filepath.Abs(out)
	if err != nil {
		return err
	}
	parent := filepath.Dir(out)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("准备输出目录: %w", err)
	}
	stage, err := os.MkdirTemp(parent, ".craft-build-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(stage)) }()
	if err := populate(root, stage, version, targets, build); err != nil {
		return err
	}
	if err := validate(stage, version, targets, report); err != nil {
		return err
	}
	return publish(stage, out)
}
