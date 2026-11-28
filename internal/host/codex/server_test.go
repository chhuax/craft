package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/host"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "app-server" {
		if err := serveFixture(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func serveFixture() error {
	f := fixtureThreads{dir: os.Getenv("CODEX_HOME")}
	ready := false
	initialized := false
	input := bufio.NewScanner(os.Stdin)
	send := json.NewEncoder(os.Stdout)
	for input.Scan() {
		var req struct {
			ID     int
			Method string
			Params json.RawMessage
		}
		if err := json.Unmarshal(input.Bytes(), &req); err != nil {
			return err
		}
		var result any
		switch req.Method {
		case "initialize":
			if initialized {
				return errors.New("重复握手")
			}
			var p struct{ ClientInfo struct{ Name string } }
			if err := json.Unmarshal(req.Params, &p); err != nil || p.ClientInfo.Name != "craft" {
				return errors.New("握手参数错误")
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "reject")); err == nil {
				return errors.New("拒绝握手")
			}
			initialized = true
			result = map[string]string{"userAgent": "fixture"}
		case "initialized":
			if !initialized || req.ID != 0 {
				return errors.New("通知顺序错误")
			}
			ready = true
			continue
		default:
			if !ready {
				return errors.New("握手前收到请求")
			}
			switch req.Method {
			case "disconnect":
				return nil
			case "pid":
				result = os.Getpid()
			default:
				var err error
				result, err = f.handle(req.Method, req.Params)
				if errors.Is(err, errDisconnect) {
					return nil
				}
				if err != nil {
					if e := send.Encode(map[string]any{"id": req.ID, "error": map[string]any{"code": -32000, "message": err.Error()}}); e != nil {
						return e
					}
					continue
				}
			}
		}
		if err := send.Encode(map[string]any{"id": req.ID, "result": result}); err != nil {
			return err
		}
	}
	return input.Err()
}

func TestLongConnectionAndReconnect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRAFT_HOME", dir)
	t.Setenv("CODEX_HOME", dir)
	s := newServer(os.Args[0])
	t.Cleanup(func() { _ = s.close() })
	now := time.Unix(0, 0)
	s.now = func() time.Time { return now }
	var first, second int
	if err := s.call(t.Context(), "pid", nil, &first); err != nil {
		t.Fatal(err)
	}
	if err := s.call(t.Context(), "pid", nil, &second); err != nil || second != first {
		t.Fatal(first, second, err)
	}
	if err := s.call(t.Context(), "disconnect", nil, nil); err == nil {
		t.Fatal("丢失响应仍确认")
	}
	if err := s.call(t.Context(), "pid", nil, &second); !errors.Is(err, host.Later) {
		t.Fatal(err)
	}
	s.mu.Lock()
	now = now.Add(time.Second)
	s.mu.Unlock()
	if err := s.call(t.Context(), "pid", nil, &second); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if err := s.call(t.Context(), "pid", nil, &second); err == nil {
		t.Fatal("关闭后重启了服务")
	}
}
func TestHandshakeFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRAFT_HOME", dir)
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "reject"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	s := newServer(os.Args[0])
	defer func() { _ = s.close() }()
	if err := s.call(context.Background(), "pid", nil, nil); !errors.Is(err, host.Later) {
		t.Fatal(err)
	}
}
