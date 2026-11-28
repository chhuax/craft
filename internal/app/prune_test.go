package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
)

func TestPruneScratchPaths(t *testing.T) {
	state := t.TempDir()
	base := filepath.Join(state, "scratch")
	checkErr(t, os.Mkdir(base, 0700))
	keep := filepath.Join(base, "keep")
	checkErr(t, os.Mkdir(keep, 0700))
	for _, id := range []string{"", ".", "..", "sub/../keep", `sub\..\keep`, "missing"} {
		checkErr(t, removeScratch(state, id))
	}
	_, err := os.Stat(keep)
	checkErr(t, err)
	dir := filepath.Join(base, "ended")
	checkErr(t, os.Mkdir(dir, 0700))
	checkErr(t, os.WriteFile(filepath.Join(dir, "nested"), nil, 0600))
	checkErr(t, removeScratch(state, "ended"))
	checkErr(t, removeScratch(state, "ended"))
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

func TestPruneScratchSymlinks(t *testing.T) {
	state, outside := t.TempDir(), t.TempDir()
	base := filepath.Join(state, "scratch")
	victim := filepath.Join(outside, "ended")
	checkErr(t, os.Mkdir(victim, 0700))
	if err := os.Symlink(outside, base); err != nil {
		t.Skipf("当前环境不能创建符号链接: %v", err)
	}
	if err := removeScratch(state, "ended"); err == nil {
		t.Fatal("接受了逃逸的 scratch 根目录")
	}
	_, err := os.Stat(victim)
	checkErr(t, err)
	checkErr(t, os.Remove(base))
	checkErr(t, os.Mkdir(base, 0700))
	link := filepath.Join(base, "ended")
	checkErr(t, os.Symlink(victim, link))
	checkErr(t, removeScratch(state, "ended"))
	if _, err := os.Lstat(link); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	_, err = os.Stat(victim)
	checkErr(t, err)
}

func TestPruneAlarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := &passClock{}
		state := t.TempDir()
		d := NewDaemon(DaemonConfig{StateDir: state, Clock: clock, AlarmOverrides: map[string]AlarmOverride{"watch.prune": {Every: time.Second}}})
		d.db = inboxDB(t, filepath.Join(state, "db"))
		d.runtime = testRuntime(t, d.db)
		ctx := t.Context()
		at := clock.Now().Add(time.Second - 90*24*time.Hour).UTC()
		checkErr(t, d.runtime.repo.Atomic(ctx, func(u engine.Unit) error {
			for _, w := range []engine.Work{{ID: "old", Ended: &engine.WorkEnd{At: at.Add(-time.Millisecond).Format(utcMillis)}}, {ID: "open"}, {ID: "boundary", Ended: &engine.WorkEnd{At: at.Format(utcMillis)}}} {
				checkErr(t, u.PutWork(w))
			}
			triggerAt := clock.Now().Add(time.Second - 30*24*time.Hour).UTC()
			checkErr(t, u.PutTrigger(engine.Trigger{ID: "old", At: triggerAt.Add(-time.Millisecond).Format(utcMillis)}))
			return u.PutTrigger(engine.Trigger{ID: "boundary", At: triggerAt.Format(utcMillis)})
		}))
		for _, id := range []string{"old", "open", "boundary"} {
			checkErr(t, os.MkdirAll(filepath.Join(state, "scratch", id), 0700))
		}
		checkErr(t, d.startAlarms(ctx, nil))
		defer func() { checkErr(t, d.alarms.Stop(ctx)) }()
		clock.advance(time.Second - time.Millisecond)
		_, err := os.Stat(filepath.Join(state, "scratch", "old"))
		checkErr(t, err)
		clock.advance(time.Millisecond)
		for _, id := range []string{"old", "open", "boundary"} {
			_, err := os.Stat(filepath.Join(state, "scratch", id))
			if (id == "old" && !errors.Is(err, os.ErrNotExist)) || (id != "old" && err != nil) {
				t.Fatalf("拨钟后 scratch/%s: %v", id, err)
			}
		}
		checkErr(t, d.runtime.repo.Read(ctx, func(r engine.Reader) error {
			triggers, err := r.GetTriggers("old", "boundary")
			if err == nil && (len(triggers) != 1 || triggers["boundary"] == nil) {
				t.Fatal("闹钟事件保留期错误", triggers)
			}
			return err
		}))
	})
}

type pruneAbort struct {
	engine.Store
	beforeCommit func() error
}

func (s pruneAbort) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return s.Store.Atomic(ctx, func(u engine.Unit) error {
		if err := fn(u); err != nil {
			return err
		}
		return s.beforeCommit()
	})
}

func TestPruneScratchCommit(t *testing.T) { synctest.Test(t, pruneScratchCommit) }

func pruneScratchCommit(t *testing.T) {
	var logs bytes.Buffer
	state := t.TempDir()
	d := NewDaemon(DaemonConfig{StateDir: state, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	d.db = inboxDB(t, filepath.Join(state, "db"))
	d.runtime = testRuntime(t, d.db)
	ctx := t.Context()
	checkErr(t, d.runtime.repo.Atomic(ctx, func(u engine.Unit) error {
		return u.PutWork(engine.Work{ID: "old", Ended: &engine.WorkEnd{At: d.cfg.Clock.Now().Add(-91 * 24 * time.Hour).UTC().Format(utcMillis)}})
	}))
	dir := filepath.Join(state, "scratch", "old")
	checkErr(t, os.MkdirAll(dir, 0700))
	abort := errors.New("撤销提交")
	pass := d.runtime.pass
	var err error
	d.runtime.pass, err = d.runtime.newEngine(pruneAbort{d.runtime.repo, func() error {
		_, err := os.Stat(dir)
		checkErr(t, err)
		return abort
	}}, benchScope{})
	checkErr(t, err)
	if err := d.prune(ctx, alarm.RunRequest{}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	_, err = os.Stat(dir)
	checkErr(t, err)
	d.runtime.pass = pass
	checkErr(t, os.RemoveAll(filepath.Join(state, "scratch")))
	checkErr(t, os.WriteFile(filepath.Join(state, "scratch"), nil, 0600))
	checkErr(t, d.prune(ctx, alarm.RunRequest{}))
	if logs.Len() == 0 {
		t.Fatal("目录清理失败未记日志")
	}
	checkErr(t, d.runtime.repo.Read(ctx, func(r engine.Reader) error {
		w, err := r.GetWork("old")
		if w != nil {
			t.Fatal("目录失败撤销了库清理")
		}
		return err
	}))
}

func TestPruneInvalidInterval(t *testing.T) {
	d := NewDaemon(DaemonConfig{StateDir: t.TempDir(), AlarmOverrides: map[string]AlarmOverride{"watch.prune": {Every: -time.Second}}})
	if err := d.Start(t.Context()); err == nil {
		checkErr(t, d.Stop(t.Context()))
		t.Fatal("接受了负清理间隔")
	}
}
