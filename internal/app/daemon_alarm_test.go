package app

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/exthost"
)

func TestDaemonRequestedAlarm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		clock := &passClock{}
		ctx := t.Context()
		path := filepath.Join(t.TempDir(), "db")
		db, client := inboxDB(t, path), inboxDB(t, path)
		service, err := alarm.NewService(alarm.Config{Clock: clock, GlobalLimit: 1})
		checkErr(t, err)
		q := newAlarmQueue(db, service)
		entered := make(chan struct{}, 4)
		released := make(chan struct{})
		canceled := make(chan struct{})
		r1, w1 := io.Pipe()
		r2, w2 := io.Pipe()
		remote := exthost.NewConn(r1, w2, func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
			entered <- struct{}{}
			select {
			case <-released:
			case <-ctx.Done():
				close(canceled)
				<-released
			}
			return nil, nil
		}, 0)
		local := exthost.NewConn(r2, w1, nil, 0)
		defer func() { checkErr(t, local.Close()); checkErr(t, remote.Close()) }()
		checkErr(t, service.Register(alarm.AlarmSpec{ID: "ext:poll", Group: "ext", Every: time.Second, Timeout: time.Second, Run: q.wrap("ext:poll", func(ctx context.Context, req alarm.RunRequest) error {
			return local.CallWaiting(ctx, "alarm/run", req, nil)
		})}))
		checkErr(t, service.Start(ctx))
		first, err := EnqueueAlarm(ctx, client, "ext", "poll")
		checkErr(t, err)
		state := func(id, want string) {
			t.Helper()
			r, err := ReadAlarmRequest(ctx, client, id)
			checkErr(t, err)
			if r.Status != want {
				t.Fatalf("请求 %s 状态 %s，期望 %s", id, r.Status, want)
			}
		}
		state(first, "queued")
		checkErr(t, q.poll(ctx))
		<-entered
		state(first, "running")
		clock.advance(time.Second)
		<-canceled
		synctest.Wait()
		s := service.List()[0]
		if s.ConsecutiveFailures != 1 || s.State != "running" {
			t.Fatal(s)
		}
		second, err := EnqueueAlarm(ctx, client, "ext", "poll")
		checkErr(t, err)
		checkErr(t, q.poll(ctx))
		state(first, "failed")
		state(second, "skipped")
		clock.advance(2 * time.Second)
		s = service.List()[0]
		if s.LastResult != "skipped" || s.State != "running" {
			t.Fatal(s)
		}
		close(released)
		synctest.Wait()
		clock.advance(time.Second)
		select {
		case <-entered:
		default:
			t.Fatal("释放名额后未恢复周期运行")
		}
		third, err := EnqueueAlarm(ctx, client, "ext", "poll")
		checkErr(t, err)
		checkErr(t, q.poll(ctx))
		synctest.Wait()
		checkErr(t, q.poll(ctx))
		state(third, "done")
		missing, err := EnqueueAlarm(ctx, client, "ext", "missing")
		checkErr(t, err)
		checkErr(t, q.poll(ctx))
		state(missing, "failed")
		checkErr(t, service.Stop(ctx))
	})
}
