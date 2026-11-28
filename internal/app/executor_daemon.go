package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/host/claudecode"
	"github.com/chhuax/craft/internal/host/codex"
	"github.com/chhuax/craft/internal/store"
)

func (d *Daemon) setupExecutor() error {
	x := newExecutor(d.runtime, d.cfg.StateDir, "", map[string]host.Driver{}, d.health)
	d.executor = x
	var config struct {
		Host  string `json:"host"`
		Watch struct {
			Executor struct {
				MaxWorkers                              int
				MergeWindowSeconds, MergeMaxWaitSeconds *int
			}
		}
	}
	raw, err := os.ReadFile(filepath.Join(d.cfg.StateDir, "host.json"))
	if err == nil {
		err = json.Unmarshal(raw, &config)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("读取宿主选择: %w", err)
	}
	settings := config.Watch.Executor
	if settings.MaxWorkers > 0 {
		x.maxWorkers = settings.MaxWorkers
	}
	if s := settings.MergeWindowSeconds; s != nil && *s >= 0 {
		x.window = time.Duration(*s) * time.Second
	}
	if s := settings.MergeMaxWaitSeconds; s != nil && *s >= 0 {
		x.maxWait = time.Duration(*s) * time.Second
	}
	for _, name := range []string{host.ClaudeCode, "codex"} {
		cmd := name
		if name == host.ClaudeCode {
			cmd = "claude"
		}
		program, err := exec.LookPath(cmd)
		if err != nil {
			d.health.report("executor", name, HealthEntry{"unavailable", err.Error()})
			continue
		}
		if name == host.ClaudeCode {
			x.drivers[name] = claudecode.New(program, x.lookupSession)
		} else {
			x.drivers[name] = codex.New(program)
		}
		if x.name == "" {
			x.name = name
		}
	}
	if config.Host != "" {
		x.name = config.Host
		if x.drivers[x.name] == nil {
			d.health.report("executor", x.name, HealthEntry{"unavailable", "配置的宿主不可用"})
		}
	}
	return nil
}
func (x *executor) lookupSession(ctx context.Context, s host.Session) (spec host.CreateSpec, err error) {
	err = x.r.repo.db.View(ctx, func(r store.SQLiteReader) error {
		ws, err := (engineReader{r}).works("sessionWorks", s.Host, s.ID)
		if err != nil {
			return err
		}
		if len(ws) == 0 || len(ws) > 1 && ws[1].Ended == nil || latest(&ws[0]) == nil {
			return errStale
		}
		w := &ws[0]
		a := latest(w)
		// 旧记录可能只有会话和 running 派工。刚换代的 pending 必须先完成本代登记。
		legacy := w.Execution.Assignment == "" && a.State == engine.AssignmentRunning
		if a.State == engine.AssignmentEnded || !legacy && w.Execution.Assignment != a.ID {
			return errStale
		}
		spec = host.CreateSpec{Directory: w.Execution.Directory, Permissions: workerPermissions(w.Execution.Target), Env: map[string]string{host.EnvWork: w.ID, host.EnvAssignment: a.ID}}
		return nil
	})
	return
}
func (x *executor) close() error {
	var errs []error
	for _, driver := range x.drivers {
		if d, ok := driver.(io.Closer); ok {
			errs = append(errs, d.Close())
		}
	}
	return errors.Join(errs...)
}
func (x *executor) monitor(ctx context.Context, _ alarm.RunRequest) error {
	err := x.sample(ctx, false)
	x.r.effects.Kick()
	return err
}
