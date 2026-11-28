package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "echo-env":
			fmt.Println(strings.Join(os.Environ(), "\n"))
			_, _ = io.Copy(os.Stdout, os.Stdin)
			os.Exit(0)
		case "oversize-out", "oversize-err":
			f := os.Stdout
			if os.Args[1] == "oversize-err" {
				f = os.Stderr
			}
			_, _ = io.WriteString(f, strings.Repeat("x", outputLimit+1))
			<-time.After(time.Hour)
		case "background":
			leaf := exec.Command(os.Args[0], "background-leaf")
			if err := leaf.Start(); err != nil {
				os.Exit(2)
			}
			fmt.Println(leaf.Process.Pid)
			_ = leaf.Process.Release()
			os.Exit(0)
		case "background-leaf":
			// 文件充当跨平台握手，只有启动器返回后父测试才发 released。
			limit := time.NewTimer(10 * time.Second)
			tick := time.NewTicker(5 * time.Millisecond)
			for {
				select {
				case <-limit.C:
					os.Exit(2)
				case <-tick.C:
				}
				if _, err := os.Stat("released"); err == nil {
					if err := os.WriteFile("observed", nil, 0600); err != nil {
						os.Exit(2)
					}
					os.Exit(0)
				}
			}
		case "tree":
			child := exec.Command(os.Args[0], "leaf")
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			if err := child.Start(); err != nil {
				os.Exit(2)
			}
			_ = child.Wait()
			os.Exit(0)
		case "leaf":
			fmt.Println(os.Getpid())
			<-time.After(time.Hour)
		}
	}
	os.Exit(m.Run())
}

func TestRunFiltersEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CRAFT_HOME", dir)
	t.Setenv("PRIVATE_TOKEN", "parent-secret")
	t.Setenv(EnvSessionID, "stale-session")
	env := BindingEnv(Session{Host: "test"}, map[string]string{EnvWork: "work", EnvAssignment: "a", EnvHost: "fake-host", EnvSessionID: "forged", "PATH": "fake-path"})
	prompt := strings.Repeat("-文字", 20000)
	out, _, err := Run(t.Context(), os.Args[0], dir, []string{"echo-env"}, env, prompt, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{"parent-secret", "stale-session", "forged", "fake-host", "fake-path"} {
		if strings.Contains(out, part) {
			t.Fatalf("子进程收到了 %q", part)
		}
	}
	for _, part := range []string{prompt, EnvWork + "=work", EnvAssignment + "=a", EnvHost + "=test"} {
		if !strings.Contains(out, part) {
			t.Error("子进程缺少正文或绑定变量")
		}
	}
}

func TestOutputLimitTerminates(t *testing.T) {
	for _, mode := range []string{"oversize-out", "oversize-err"} {
		t.Run(mode, func(t *testing.T) {
			_, _, err := Run(t.Context(), os.Args[0], t.TempDir(), []string{mode}, nil, "", 10*time.Second)
			if !errors.Is(err, errOutputLimit) {
				t.Fatal(err)
			}
		})
	}
}

func tree(t *testing.T, ctx context.Context) (*Process, int) {
	t.Helper()
	p, err := Start(ctx, os.Args[0], t.TempDir(), []string{"tree"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	line, err := bufio.NewReader(p.Stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return p, pid
}

func TestCancelAfterChildStarted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	p, _ := tree(t, ctx)
	cancel()
	if err := p.Wait(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestTimeout(t *testing.T) {
	_, _, err := Run(t.Context(), os.Args[0], t.TempDir(), []string{"leaf"}, nil, "", 100*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestSuccessfulBackgroundHandoff(t *testing.T) {
	dir := t.TempDir()
	if _, err := LaunchBackground(t.Context(), os.Args[0], dir, []string{"background"}, nil, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "released"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(filepath.Join(dir, "observed")); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("成功的启动器退出后后台会话未存活")
		case <-tick.C:
		}
	}
}

func TestStalledCommandsHoldSlots(t *testing.T) {
	dir := t.TempDir()
	for range cap(processSlots) {
		p, err := Start(t.Context(), os.Args[0], dir, []string{"leaf"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	p, err := Start(ctx, os.Args[0], dir, []string{"echo-env"}, nil)
	if p != nil {
		_ = p.Close()
		t.Fatal("卡住的命令没有占住名额")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
