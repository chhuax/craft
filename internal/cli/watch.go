package cli

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
)

func threadRef(value string) (engine.ThreadRef, error) {
	if !strings.HasPrefix(strings.TrimSpace(value), "{") {
		return threadKeyRef(value)
	}
	var ref engine.ThreadRef
	err := jsonv2.Unmarshal([]byte(value), &ref, jsonv2.RejectUnknownMembers(true))
	if err != nil {
		return ref, invalid("无效的 ThreadRef: %v", err)
	}
	if ref.Source() == "" || ref.Kind() == "" || ref.ID() == "" || strings.Contains(ref.Source()+ref.Kind(), ":") {
		return ref, invalid("对象引用缺少分量或无法还原对象键")
	}
	return ref, nil
}

func threadKeyRef(key string) (engine.ThreadRef, error) {
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return engine.ThreadRef{}, invalid("需要 source:kind:id 形式的对象引用")
	}
	return engine.NewThreadRef(parts[0], parts[1], parts[2]), nil
}

func (i *invocation) write(fn func(*engine.Engine, string) (any, error)) (out result, err error) {
	var scope engine.SourceScope
	if i.extensionDirs != nil && (i.spec.path == "watch request" || i.spec.path == "watch attach") {
		kinds, err := app.LoadObjectKinds(i.extensionDirs)
		if err != nil {
			return out, fmt.Errorf("%w: %w", errDependency, err)
		}
		scope = kinds
	}
	c, err := app.OpenCommands(i.ctx, i.home, i.clock, scope)
	if err != nil {
		return out, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	err = c.Execute(i.ctx, i.binding, func(e *engine.Engine, id string) error {
		v, err := fn(e, id)
		if err != nil {
			return err
		}
		switch value := v.(type) {
		case engine.Work:
			out.payload = map[string]string{"workId": value.ID}
		case *engine.Work:
			if value == nil {
				return &engine.UserError{Code: engine.CodeAssignmentEnded}
			}
			out.payload = map[string]string{"workId": value.ID}
		case engine.Decision:
			out.payload = map[string]string{"decisionId": value.ID}
		default:
			out.payload = v
		}
		return nil
	})
	return out, err
}

func request(i *invocation) (result, error) {
	ref, err := threadRef(i.args[0])
	if err != nil {
		return result{}, err
	}
	return i.write(func(e *engine.Engine, _ string) (any, error) { return e.RequestItem(i.ctx, ref, i.flags["--text"]) })
}
func stop(i *invocation) (result, error) {
	return i.write(func(e *engine.Engine, _ string) (any, error) { return e.StopWork(i.ctx, i.args[0], "") })
}
func answer(i *invocation) (result, error) {
	return i.write(func(e *engine.Engine, _ string) (any, error) { return e.Answer(i.ctx, i.args[0], i.flags["--text"]) })
}
func retry(i *invocation) (result, error) {
	return i.write(func(e *engine.Engine, _ string) (any, error) { return e.RetryWork(i.ctx, i.args[0]) })
}
func reevaluate(i *invocation) (result, error) {
	return i.write(func(e *engine.Engine, _ string) (any, error) {
		ok, err := e.Reevaluate(i.ctx, i.args[0])
		return map[string]bool{"queued": ok}, err
	})
}
func attach(i *invocation) (result, error) {
	return i.write(func(e *engine.Engine, _ string) (any, error) { return e.Attach(i.ctx, i.args[0], i.args[1], "") })
}
func unsubscribe(i *invocation) (result, error) {
	ref, err := threadKeyRef(i.args[0])
	if err != nil {
		return result{}, err
	}
	return i.write(func(e *engine.Engine, _ string) (any, error) {
		ok, err := e.Unsubscribe(i.ctx, engine.ThreadKey(ref))
		return map[string]bool{"changed": ok}, err
	})
}
func dismiss(i *invocation) (result, error) {
	return i.write(func(e *engine.Engine, _ string) (any, error) { return e.Dismiss(i.ctx, i.args[0]) })
}
func report(i *invocation) (result, error) {
	req := engine.WorkerReport{State: engine.ReportState(i.args[0]), Summary: i.flags["--summary"], Session: &i.binding.Session}
	switch req.State {
	case engine.Progress, engine.Ready, engine.Blocked, engine.Failed, engine.ReportDropped:
	default:
		return result{}, invalid("执行方不能报告状态 %q", req.State)
	}
	if q, ok := i.flags["--question"]; ok {
		raw, err := json.Marshal(q)
		if err != nil {
			return result{}, err
		}
		req.HumanAction = raw
	}
	return i.write(func(e *engine.Engine, id string) (any, error) { return e.Report(i.ctx, id, i.binding.Assignment, req) })
}
func handback(i *invocation) (result, error) {
	reason := i.flags["--reason"]
	if utf8.RuneCountInString(reason) > 300 {
		return result{}, invalid("退回理由超过 300 字")
	}
	return i.write(func(e *engine.Engine, id string) (any, error) {
		ok, err := e.Handback(i.ctx, id, i.args[0], reason)
		return map[string]bool{"queued": ok}, err
	})
}
func followContext(i *invocation) (result, error) {
	ref, err := threadRef(i.args[0])
	if err != nil {
		return result{}, err
	}
	return i.write(func(e *engine.Engine, id string) (any, error) {
		ok, err := e.FollowForContext(i.ctx, id, ref, "")
		return map[string]bool{"changed": ok}, err
	})
}
func markUnrelated(i *invocation) (result, error) {
	ref, err := threadKeyRef(i.args[0])
	if err != nil {
		return result{}, err
	}
	return i.write(func(e *engine.Engine, id string) (any, error) {
		return map[string]string{"workId": id}, e.MarkUnrelated(i.ctx, id, ref, "")
	})
}
func note(i *invocation) (result, error) {
	return i.write(func(*engine.Engine, string) (any, error) {
		return nil, fmt.Errorf("%w: 当前引擎没有执行方 note 命令", errDependency)
	})
}

func setPolicy(i *invocation) (_ result, err error) {
	c, err := app.OpenCommands(i.ctx, i.home, i.clock, nil)
	if err != nil {
		return result{}, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	p, err := c.SetPolicy(i.ctx, []byte(i.args[0]))
	return result{payload: p}, err
}

func turnEnded(i *invocation) (_ result, err error) {
	c, err := app.OpenCommands(i.ctx, i.home, i.clock, nil)
	if err != nil {
		return result{}, err
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	if err = c.WorkerTurnEnded(i.ctx, *i.binding); err != nil {
		return result{}, err
	}
	return data(map[string]bool{"accepted": true})
}
