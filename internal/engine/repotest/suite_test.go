package repotest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

// 故障替身仅替换待检行为，其他操作仍走真实仓储。
type brokenStore struct {
	engine.Store
	fault string
}

type sharedReader struct {
	engine.Reader
	thread *engine.Thread
}

func (r *sharedReader) GetThread(k string) (*engine.Thread, error) {
	if r.thread == nil {
		v, err := r.Reader.GetThread(k)
		if err != nil {
			return nil, err
		}
		r.thread = v
	}
	return r.thread, nil
}

func (s brokenStore) Read(ctx context.Context, fn func(engine.Reader) error) error {
	return s.Store.Read(ctx, func(r engine.Reader) error {
		if s.fault == "ownership" {
			return fn(&sharedReader{Reader: r})
		}
		return fn(r)
	})
}

type noSavepoint struct{ engine.Unit }

func (u noSavepoint) Atomic(_ context.Context, fn func(engine.Unit) error) error { return fn(u.Unit) }

func (s brokenStore) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return s.Store.Atomic(ctx, func(u engine.Unit) error {
		if s.fault == "savepoints" {
			return fn(noSavepoint{u})
		}
		return fn(u)
	})
}

func TestDetectBrokenStore(t *testing.T) {
	const mode = "CRAFT_REPOTEST_FAULT"
	if fault := os.Getenv(mode); fault != "" {
		Run(t, func(*testing.T) engine.Store { return brokenStore{engine.NewMemory(), fault} })
		return
	}
	// testing.T 无法撤销 Fail；在子进程里让真实的 Run 失败，再核对失败的契约名。
	for _, fault := range []string{"ownership", "savepoints"} {
		t.Run(fault, func(t *testing.T) {
			bin, err := os.Executable()
			must(t, err)
			cmd := exec.CommandContext(t.Context(), bin, "-test.run=^TestDetectBrokenStore$/^"+fault+"$", "-test.v")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), mode+"="+fault)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 || !strings.Contains(string(out), "--- FAIL: TestDetectBrokenStore/"+fault+" ") {
				t.Fatalf("未检出 %s 故障：%v\n%s", fault, err, out)
			}
		})
	}
}
