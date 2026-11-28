package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/app"
)

func daemonRun(i *invocation) (_ result, err error) {
	signals, release := signal.NotifyContext(i.ctx, daemonSignals...)
	defer release()
	ctx, cancel := context.WithCancel(signals)
	defer cancel()
	lease, err := claimDaemon(ctx, i.home)
	if err != nil {
		return result{}, err
	}
	defer func() { err = errors.Join(err, lease.close()) }()
	// 默认根目录即使首次为空，也必须进入指纹，随后添加第一个扩展才能触发换代。
	if err = os.MkdirAll(filepath.Join(i.home, "extensions"), 0700); err != nil {
		return result{}, err
	}
	dirs := append([]string{filepath.Join(i.home, "extensions")}, i.extensionDirs...)
	logger := slog.New(slog.NewTextHandler(i.stderr, nil))
	hub := newEventHub()
	d := app.NewDaemon(app.DaemonConfig{StateDir: i.home, Version: i.version, ExtensionDirs: dirs, Logger: logger, OnEvent: hub.notify})
	if err = d.Start(ctx); err != nil {
		return result{}, fmt.Errorf("启动 daemon: %w", err)
	}
	defer func() { err = errors.Join(err, d.Stop(context.Background())) }()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return result{}, fmt.Errorf("监听回环地址: %w", err)
	}
	st := daemonState{PID: os.Getpid(), Port: ln.Addr().(*net.TCPAddr).Port, Version: i.version, StartedAt: time.Now().UTC(), Token: rand.Text()}
	api := localAPI{state: st, home: i.home, daemon: d, events: hub, stop: cancel, force: func() { os.Exit(1) }}
	serving, end := context.WithCancel(context.Background())
	defer end()
	go hub.run(serving)
	server := api.server(serving)
	done := make(chan error, 1)
	go func() { done <- server.Serve(cappedListener{ln, make(chan struct{}, connectionLimit)}) }()
	defer func() {
		end()
		grace, finish := context.WithTimeout(context.Background(), 6*time.Second)
		defer finish()
		if e := server.Shutdown(grace); e != nil {
			err = errors.Join(err, e, server.Close())
		}
	}()
	if err = st.publish(i.home); err != nil {
		return result{}, fmt.Errorf("发布 daemon 地址: %w", err)
	}
	logger.Info("daemon 已就绪", "pid", st.PID, "address", st.address())
	select {
	case <-ctx.Done():
		logger.Info("正在停止闹钟、Pass、effect 与扩展")
		// 保留已鉴权的停止入口直到 D1 退出，宽限期过后也只能由本实例执行强制退出。
		if err = d.Stop(context.Background()); err != nil {
			return result{}, err
		}
	case <-d.Done():
		logger.Info("daemon 装载周期结束")
	case err = <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			return result{}, fmt.Errorf("daemon 接口停止: %w", err)
		}
	}
	return data(map[string]bool{"stopped": true})
}

func daemonStart(i *invocation) (result, error) {
	ctx, cancel := context.WithTimeout(i.ctx, time.Minute)
	defer cancel()
	if err := os.MkdirAll(i.home, 0700); err != nil {
		return result{}, err
	}
	var child <-chan error
	pid := 0
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	for {
		var probeErr error
		if s, err := readDaemon(i.home); err == nil {
			if _, probeErr = s.status(ctx); probeErr == nil {
				out := map[string]any{"running": true, "alreadyRunning": s.PID != pid, "pid": s.PID}
				if s.PID != pid {
					out["message"] = "daemon 已在运行"
				}
				return data(out)
			}
		}
		// 持有者可能在发布地址前崩溃。等待期间重新检查内核锁，不能只在进入 start 时检查一次。
		if child == nil && ctx.Err() == nil {
			guard, err := guardFile(filepath.Join(i.home, "daemon.guard"))
			switch {
			case err == nil:
				if err := guard.Close(); err != nil {
					return result{}, err
				}
				pid, child, err = launchDaemon(i)
				if err != nil {
					return result{}, err
				}
			case !errors.Is(err, errOwned):
				return result{}, err
			case errors.Is(probeErr, errDaemonAuth):
				return result{}, probeErr
			}
		}
		select {
		case <-ctx.Done():
			return result{}, fmt.Errorf("%w: 等待 daemon 就绪超时，请检查 daemon.log", errDependency)
		case err := <-child:
			child = nil
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 6 {
				return result{}, fmt.Errorf("daemon 在就绪前退出 (%v)，请检查 daemon.log", err)
			}
		case <-tick.C:
		}
	}
}

func launchDaemon(i *invocation) (_ int, _ <-chan error, err error) {
	exe, err := os.Executable()
	if err != nil {
		return 0, nil, err
	}
	f, err := os.OpenFile(filepath.Join(i.home, "daemon.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return 0, nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Env = append(os.Environ(), "CRAFT_HOME="+i.home, "CRAFT_EXTENSIONS="+strings.Join(i.extensionDirs, string(os.PathListSeparator)))
	cmd.Stdout, cmd.Stderr = f, f
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, nil, fmt.Errorf("拉起后台 daemon: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return cmd.Process.Pid, done, nil
}

func daemonStop(i *invocation) (_ result, err error) {
	s, _, err := ensureFresh(i)
	if err != nil {
		return result{}, err
	}
	process, err := observeExit(s.PID)
	if err != nil {
		return result{}, fmt.Errorf("登记进程退出通知: %w", err)
	}
	defer func() { err = errors.Join(err, process.close()) }()
	if _, err = s.call(i.ctx, "daemon.stop", nil); err != nil {
		return result{}, err
	}
	var escalate func() error
	if _, ok := i.flags["--force"]; ok {
		// 令牌绑定这一代，禁止在旧 PID 被复用时误杀其他进程。
		escalate = func() error { _, err := s.call(i.ctx, "daemon.stop", map[string]bool{"force": true}); return err }
	}
	if err := waitForStop(i.ctx, process, escalate); err != nil {
		return result{}, err
	}
	return data(map[string]bool{"stopped": true})
}

func waitForStop(ctx context.Context, process exitProbe, force func() error) error {
	grace, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := process.wait(grace)
	if !errors.Is(err, context.DeadlineExceeded) || force == nil || ctx.Err() != nil {
		return err
	}
	// 强退可能在响应送出前关闭连接。已登记的退出通知仍是最终依据。
	callErr := force()
	final, finish := context.WithTimeout(ctx, 4*time.Second)
	defer finish()
	if err := process.wait(final); err != nil {
		return errors.Join(err, callErr)
	}
	return nil
}

func ensureFresh(i *invocation) (daemonState, json.RawMessage, error) {
	s, err := readDaemon(i.home)
	if err != nil {
		return s, nil, fmt.Errorf("%w: daemon 未运行或状态文件无效", errDependency)
	}
	raw, err := s.status(i.ctx)
	if err != nil {
		return s, nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		return s, nil, err
	}
	if s.Version != i.version || string(fields["fingerprintStale"]) == "true" {
		if _, err := fmt.Fprintln(i.stderr, "daemon 指纹已过期，请在合适时重启"); err != nil {
			return s, nil, err
		}
		fields["fingerprintStale"] = json.RawMessage("true")
		raw, err = json.Marshal(fields)
	}
	return s, raw, err
}

func daemonStatus(i *invocation) (result, error) {
	_, raw, err := ensureFresh(i)
	return result{payload: raw}, err
}
