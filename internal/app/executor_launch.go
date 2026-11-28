package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

func (x *executor) launch(ctx context.Context, p assignmentRef, attempts int) error {
	w, err := x.current(ctx, p)
	// 旧会话的 stop 只阻止开工；已交给人或缺少目标的派工应先收尾。
	if err != nil && !errors.Is(err, errStopPending) {
		return err
	}
	if latest(w).State != engine.AssignmentPending || engine.WorkStage(*w).Stage == "person" {
		return nil
	}
	if w.Execution == nil || w.Execution.Target == nil {
		return x.launchFailed(ctx, p, "没有执行目标：该 Work 缺少执行目标（可能是损坏或旧数据），请重试或停止")
	}
	if err != nil {
		return err
	}
	if err := x.reserve(ctx, w.ID, false); err != nil {
		return err
	}
	defer x.free(w.ID)
	err = x.dispatchAssignment(ctx, w, p)
	if err == nil || ctx.Err() != nil || errors.Is(err, errStale) || errors.Is(err, host.Later) && !errors.Is(err, errTransient) {
		return err
	}
	if errors.Is(err, errTransient) && attempts < 3 {
		return err
	}
	var failure *launchFailure
	if errors.As(err, &failure) || errors.Is(err, errTransient) {
		return x.launchFailed(ctx, p, err.Error())
	}
	return err
}

type launchFailure struct{ error }

func (x *executor) launchFailed(ctx context.Context, p assignmentRef, summary string) error {
	_, err := x.r.Commands.Report(ctx, p.WorkID, p.AssignmentID, engine.WorkerReport{State: engine.Failed, Summary: summary})
	return err
}

func (x *executor) dispatchAssignment(ctx context.Context, w *engine.Work, p assignmentRef) error {
	s := sessionOf(w)
	name := x.name
	if s.Host != "" {
		name = s.Host
	}
	d := x.drivers[name]
	if d == nil {
		return fmt.Errorf("%w: 宿主 %q 不可用", errTransient, name)
	}
	dir := w.Execution.Directory
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		target, err := (preparer{d}).Prepare(ctx, *w.Execution.Target, *w)
		if err != nil {
			if errors.Is(err, errTransient) {
				return err
			}
			return &launchFailure{fmt.Errorf("准备派工目录: %w", err)}
		}
		dir = target.Directory
	}
	if err := x.register(ctx, p, dir, w.Execution.Session, confirmed(w)); err != nil {
		return err
	}
	w.Execution.Directory = dir
	if s.ID != "" {
		switch d.Status(ctx, s) {
		case host.UnknownState:
			// 首条还未确认时不能猜。已确认的持久会话可能只是尚未载入，
			// 此时追加由驱动核对写入权并续接，不能另开会话。
			if confirmed(w) {
				break
			}
			return host.Later
		case host.Gone:
			if err := x.register(ctx, p, dir, nil, false); err != nil {
				return err
			}
			s = host.Session{}
		}
	}
	init := s.ID == "" || !confirmed(w)
	logical := "assignment:" + w.ID + ":" + p.AssignmentID
	var title, body string
	var err error
	if init {
		logical += ":init"
		title, body, err = x.firstMessage(ctx, w)
		if err != nil {
			return err
		}
	} else {
		// 复用的宿主线程可能保留首轮环境。告诉执行方本轮声明的代次，CLI 仍按本地会话登记核对。
		body = fmt.Sprintf("派工第 %d 轮。继续前核对工作目录、版本记录与外部状态，避免重复操作。本轮调用 craft watch 命令时使用下列派工绑定。\n引用：Work %s，%s=%s", len(w.Assignments), w.ID, host.EnvAssignment, p.AssignmentID)
	}
	id := engine.DeliveryID(logical)
	key := "launch." + p.AssignmentID
	if s.ID == "" {
		var sent bool
		s, sent, err = x.create(ctx, d, p, dir, w.Execution.Target.Path, title, body, id, key)
		if err != nil {
			return err
		}
		// 登记成功后由结束事务负责 stop，迟到回执不能覆盖它的代次和重试次数。
		x.rememberTitle(w, s)
		if sent {
			return x.receipt(context.WithoutCancel(ctx), p, s, key, nil)
		}
	}
	if _, err = x.current(ctx, p); err != nil {
		return err
	}
	if d.Status(ctx, s) == host.Busy && !d.Capabilities().BusyAppend {
		return host.Later
	}
	if _, err = x.startAt(ctx, key); err != nil {
		return err
	}
	if _, err = x.current(ctx, p); err != nil {
		return err
	}
	res := d.Append(ctx, s, body, id)
	if err := x.appendResult(ctx, p, s, id, res); err != nil {
		return err
	}
	return x.receipt(context.WithoutCancel(ctx), p, s, key, nil)
}
func (x *executor) register(ctx context.Context, p assignmentRef, dir string, s *engine.Session, boot bool) error {
	ok, err := x.r.Commands.SetExecution(ctx, p.WorkID, p.AssignmentID, dir, s, boot)
	if err != nil {
		return err
	}
	if !ok {
		return errStale
	}
	return nil
}
func (x *executor) create(ctx context.Context, d host.Driver, p assignmentRef, dir, target, title, body, id, key string) (host.Session, bool, error) {
	separate := d.Capabilities().CreateSeparatesPrompt
	spec := host.CreateSpec{Directory: dir, Title: title, Env: map[string]string{host.EnvWork: p.WorkID, host.EnvAssignment: p.AssignmentID}}
	if !separate {
		spec.Prompt, spec.DeliveryID = body, id
		if _, err := x.startAt(ctx, key); err != nil {
			return host.Session{}, false, err
		}
	}
	var registered host.Session
	var registrationErr error
	spec.OnCreated = func(s host.Session) error {
		_, registrationErr = x.current(ctx, p)
		if registrationErr == nil {
			es := engine.Session(s)
			registrationErr = x.register(ctx, p, dir, &es, false)
		}
		if registrationErr != nil {
			return registrationErr
		}
		registered = s
		return nil
	}
	for range 2 {
		if _, err := x.current(ctx, p); err != nil {
			return host.Session{}, false, err
		}
		s, err := d.Create(ctx, spec)
		if registered.ID == "" && s.ID != "" {
			// 创建已发生但回调被拒，停止必须指向新拿到的会话，不能再查 Work。
			if stopErr := x.orphanStop(ctx, p, s, dir, target); stopErr != nil {
				return s, false, stopErr
			}
			if registrationErr != nil {
				return s, false, registrationErr
			}
			return s, false, &launchFailure{errors.New("宿主返回了未经登记的会话")}
		}
		if err == nil && registered.ID != "" {
			return registered, !separate, nil
		}
		if !errors.Is(err, host.Unknown) {
			if errors.Is(err, host.Unsupported) {
				return s, false, &launchFailure{err}
			}
			return s, false, errors.Join(errTransient, err)
		}
		if registered.ID != "" {
			if !separate {
				return s, false, x.unknown(ctx, p, registered, id, err)
			}
			switch d.Status(ctx, registered) {
			case host.Idle, host.Busy:
				return registered, false, nil
			}
			if err := x.register(ctx, p, dir, nil, false); err != nil {
				return s, false, err
			}
			registered = host.Session{}
		} else if lookup, ok := d.(interface {
			FindByTitle(context.Context, string, string) ([]host.Session, error)
		}); ok {
			// 标题和目录不是身份证明。候选只供人工对账，绝不据此认领或向其发消息。
			candidates, findErr := lookup.FindByTitle(ctx, dir, title)
			if len(candidates) > 0 || findErr != nil {
				x.health.report("executor", "reconcile."+p.AssignmentID, HealthEntry{"warning", fmt.Sprintf("按标题 %q 核对候选 %v（%v）", title, candidates, findErr)})
			}
		}
	}
	return host.Session{}, false, &launchFailure{errors.New("宿主没有确认会话是否已建")}
}

// 被拒的回调仍可能留下宿主会话。停止载荷与原目录保护必须一起提交。
func (x *executor) orphanStop(ctx context.Context, p assignmentRef, s host.Session, dir, target string) error {
	ctx = context.WithoutCancel(ctx)
	p.Session = s
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return x.r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
		u := x.r.repo.in(ctx, tx)
		if err := u.put("watch_kv", "stopping."+s.Host+"."+s.ID, map[string]string{"workId": p.WorkID, "targetPath": target, "directory": dir}); err != nil {
			return err
		}
		return tx.Enqueue("stop", p.AssignmentID, raw)
	})
}
func (x *executor) unknown(ctx context.Context, p assignmentRef, s host.Session, id string, cause error) error {
	reason := fmt.Sprintf("请在宿主 %s 的会话 %s 核对投递标记 %s（%v）", s.Host, s.ID, id, cause)
	if err := x.r.Commands.DeliveryFailed(context.WithoutCancel(ctx), p.WorkID, p.AssignmentID, reason); err != nil {
		return err
	}
	return errStale
}
func (x *executor) appendResult(ctx context.Context, p assignmentRef, s host.Session, id string, res host.Result) error {
	switch res.Outcome {
	case host.OK:
		return nil
	case host.Later:
		return host.Later
	case host.Unknown:
		return x.unknown(ctx, p, s, id, res.Err)
	default:
		err := fmt.Errorf("宿主追加未成功 %s: %w", id, errors.Join(res.Outcome, res.Err))
		return errors.Join(err, x.r.Commands.DeliveryFailed(context.WithoutCancel(ctx), p.WorkID, p.AssignmentID, err.Error()))
	}
}
