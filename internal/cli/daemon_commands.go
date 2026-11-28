package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

func daemonRPC(i *invocation) (result, error) {
	s, _, err := ensureFresh(i)
	if err != nil {
		return result{}, err
	}
	var params any
	if len(i.args) == 1 {
		params = map[string]string{"id": i.args[0]}
	}
	raw, err := s.call(i.ctx, strings.ReplaceAll(i.spec.path, " ", "."), params)
	return result{payload: raw}, err
}

func alarmRun(i *invocation) (result, error) {
	sec, err := i.number("--wait", 0, 0, int64((1<<63-1)/time.Second))
	if err != nil {
		return result{}, err
	}
	s, _, err := ensureFresh(i)
	if err != nil {
		return result{}, err
	}
	raw, err := s.call(i.ctx, "alarm.run", map[string]string{"ext": i.args[0], "alarm": i.args[1]})
	if err != nil {
		return result{}, err
	}
	if _, wait := i.flags["--wait"]; !wait {
		return data(raw)
	}
	var q struct {
		RequestID string `json:"requestId"`
	}
	if err := json.Unmarshal(raw, &q); err != nil || q.RequestID == "" {
		return result{}, errors.New("daemon 缺少请求 ID")
	}
	return waitAlarm(i.ctx, i.home, q.RequestID, time.Duration(sec)*time.Second)
}

func waitAlarm(ctx context.Context, home, id string, timeout time.Duration) (result, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		var req app.AlarmRequest
		err := app.ViewCommands(ctx, home, func(r app.CommandReader) error {
			var err error
			req, err = r.AlarmRequest(id)
			return err
		})
		if err != nil {
			return result{}, err
		}
		out := map[string]any{"queued": true, "requestId": id, "status": req.Status}
		if req.Status != "queued" && req.Status != "running" {
			if req.Detail != "" {
				out["detail"] = req.Detail
			}
			return data(out)
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-deadline.C:
			out["timedOut"] = true
			return data(out)
		case <-tick.C:
		}
	}
}

func extList(i *invocation) (result, error) {
	s, _, err := ensureFresh(i)
	if err == nil {
		raw, err := s.call(i.ctx, "ext.list", nil)
		return result{payload: raw}, err
	}
	if errors.Is(err, errDaemonAuth) {
		return result{}, err
	}
	path := filepath.Join(i.home, "daemon.guard")
	if _, e := os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		if e != nil {
			return result{}, e
		}
		guard, e := guardFile(path)
		if errors.Is(e, errOwned) {
			return result{}, err
		}
		if e != nil {
			return result{}, e
		}
		if e = guard.Close(); e != nil {
			return result{}, e
		}
	}
	discovered := exthost.Discover(i.extensionDirs...)
	loaded := map[string]app.HealthEntry{}
	for _, ext := range discovered.Extensions {
		loaded[ext.Manifest.ID] = app.HealthEntry{Status: "discovered"}
	}
	rejected := map[string]app.HealthEntry{}
	for p, reason := range discovered.Rejected {
		rejected[p] = app.HealthEntry{Status: "rejected", Detail: reason.Error()}
	}
	return data(map[string]any{"running": false, "message": "daemon 未运行", "extensions": loaded, "rejected": rejected})
}

func extLogs(i *invocation) (result, error) {
	n, err := i.number("--tail", 50, 1, 1000)
	if err != nil {
		return result{}, err
	}
	lines := int(n)
	page, err := logPage(i.home, i.args[0], &lines)
	if errors.Is(err, os.ErrNotExist) {
		return result{}, fmt.Errorf("%w: 扩展没有日志", errMissing)
	}
	return result{payload: page}, err
}

var errMissing = errors.New("找不到")

func rpcFailure(err error) (string, int) {
	e, ok := errors.AsType[*protocol.Error](err)
	if !ok {
		return "internalError", 1
	}
	switch e.Code {
	case -32602:
		return "invalidArguments", 2
	case -32004:
		return "missing", 5
	case -32006:
		return "unavailable", 6
	}
	return "internalError", 1
}
