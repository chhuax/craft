package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

type daemonState struct {
	PID       int       `json:"pid"`
	Port      int       `json:"port"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	Token     string    `json:"token"`
}

func (s daemonState) address() string { return "127.0.0.1:" + strconv.Itoa(s.Port) }

func readDaemon(home string) (daemonState, error) {
	var s daemonState
	raw, err := os.ReadFile(filepath.Join(home, "daemon.json"))
	if err != nil {
		return s, fmt.Errorf("读取 daemon 状态: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil || s.Port < 1 || s.Port > 65535 || s.PID <= 0 || len(s.Token) < 22 || s.StartedAt.IsZero() {
		return daemonState{}, errors.New("daemon 状态文件无效")
	}
	return s, nil
}

func (s daemonState) publish(home string) (err error) {
	f, err := os.CreateTemp(home, ".daemon-*.json")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeFile(f.Name()))
		}
	}()
	if err = errors.Join(json.NewEncoder(f).Encode(s), f.Sync(), f.Close()); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(home, "daemon.json"))
}

const maxResponse = 2 << 20

// 明确禁用代理和重定向，令牌只能送到状态文件中该端口的 IPv4 回环接口。
var rpcClient = &http.Client{Transport: &http.Transport{MaxIdleConns: 8, IdleConnTimeout: 30 * time.Second}, Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (s daemonState) call(ctx context.Context, method string, params any) (_ json.RawMessage, err error) {
	request := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		request["params"] = params
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "http://"+s.address()+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := rpcClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法连接 daemon: %v", errDependency, err)
	}
	defer func() { err = errors.Join(err, resp.Body.Close()) }()
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, errDaemonAuth
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: daemon 拒绝连接或令牌无效 (HTTP %d)", errDependency, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, fmt.Errorf("%w: 无法读取 daemon 响应", errDependency)
	}
	var reply protocol.Response
	if len(raw) > maxResponse || json.Unmarshal(raw, &reply) != nil || reply.JSONRPC != "2.0" || string(reply.ID) != "1" {
		return nil, fmt.Errorf("%w: daemon 握手响应无效", errDependency)
	}
	if reply.Error != nil {
		return nil, reply.Error
	}
	if len(reply.Result) == 0 {
		return nil, fmt.Errorf("%w: daemon 缺少结果", errDependency)
	}
	return reply.Result, nil
}

func (s daemonState) status(ctx context.Context) (json.RawMessage, error) {
	raw, err := s.call(ctx, "status", nil)
	if err != nil {
		return nil, err
	}
	var identity struct {
		PID       int
		Running   bool
		StartedAt time.Time
	}
	if json.Unmarshal(raw, &identity) != nil || !identity.Running || identity.PID != s.PID || !identity.StartedAt.Equal(s.StartedAt) {
		return nil, fmt.Errorf("%w: daemon 实例不匹配", errDependency)
	}
	return raw, nil
}

var errDaemonAuth = fmt.Errorf("%w: daemon 令牌无效", errDependency)

var errOwned = errors.New("daemon 已在运行或正在启动")

type daemonLease struct {
	home  string
	guard *os.File
}

func removeFile(p string) error {
	err := os.Remove(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func claimDaemon(ctx context.Context, home string) (_ *daemonLease, err error) {
	if err = os.MkdirAll(home, 0700); err != nil {
		return nil, err
	}
	// guard 的目录项始终存在，否则等候者可能分别锁住旧、新两个 inode。
	// 内核释放才证明上一代结束；握手失败、PID 不存在或启动超时都不足以抢锁。
	guard, err := guardFile(filepath.Join(home, "daemon.guard"))
	if errors.Is(err, errOwned) {
		if state, e := readDaemon(home); e == nil {
			if _, e = state.status(ctx); e == nil {
				return nil, fmt.Errorf("%w (pid %d)", errOwned, state.PID)
			}
			if errors.Is(e, errDaemonAuth) {
				return nil, e
			}
		}
		return nil, errOwned
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, guard.Close())
		}
	}()
	path := filepath.Join(home, "daemon.lock")
	create := func() (*os.File, error) { return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) }
	f, err := create()
	if errors.Is(err, os.ErrExist) {
		if state, e := readDaemon(home); e == nil {
			if _, e = state.status(ctx); e == nil {
				return nil, errOwned
			}
		}
		if err = removeFile(path); err != nil {
			return nil, err
		}
		f, err = create()
	}
	if err != nil {
		return nil, err
	}
	_, err = fmt.Fprintln(f, os.Getpid())
	if err = errors.Join(err, f.Close(), removeFile(filepath.Join(home, "daemon.json"))); err != nil {
		return nil, err
	}
	return &daemonLease{home, guard}, nil
}

func (l *daemonLease) close() error {
	return errors.Join(removeFile(filepath.Join(l.home, "daemon.json")), removeFile(filepath.Join(l.home, "daemon.lock")), l.guard.Close())
}

type exitProbe struct {
	exited func() (bool, error)
	close  func() error
}

func (p exitProbe) wait(ctx context.Context) error {
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	for {
		done, err := p.exited()
		if done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 daemon 退出超时: %w", ctx.Err())
		case <-tick.C:
		}
	}
}
