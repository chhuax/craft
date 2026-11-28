package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

func (x *executor) startAt(ctx context.Context, key string) (string, error) {
	var at string
	err := x.r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
		var err error
		at, err = readStart(tx, key)
		if err != nil || at != "" {
			return err
		}
		at = x.r.clock.Now().UTC().Format(utcMillis)
		return x.r.repo.in(ctx, tx).put("watch_kv", key, roundStart{at})
	})
	return at, err
}
func (x *executor) transaction(ctx context.Context, fn func(*engineUnit, *engine.Engine) error) error {
	return x.r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
		u := x.r.repo.in(ctx, tx)
		e, err := x.r.newEngine(lockedStore{x.r.repo, u}, ObjectKinds(nil))
		if err != nil {
			return err
		}
		tx.AfterCommit(x.r.Request)
		return fn(u, e)
	})
}
func (x *executor) receipt(ctx context.Context, p assignmentRef, s host.Session, key string, ids []string) error {
	x.setState(s, host.Busy)
	return x.transaction(ctx, func(u *engineUnit, e *engine.Engine) error {
		w, err := u.GetWork(p.WorkID)
		if err != nil {
			return err
		}
		a := latest(w)
		if a == nil || a.State == engine.AssignmentEnded || a.ID != p.AssignmentID || sessionOf(w) != s {
			return errStale
		}
		at, err := readStart(u.tx, key)
		if err != nil {
			return err
		}
		if ids == nil {
			var ok bool
			ok, err = e.SetExecution(ctx, w.ID, a.ID, w.Execution.Directory, w.Execution.Session, true)
			if err != nil {
				return err
			}
			if !ok {
				return errStale
			}
			err = e.AssignmentStarted(ctx, w.ID, a.ID, at)
		} else {
			err = e.InputsDelivered(ctx, w.ID, a.ID, ids, true, at)
		}
		if err != nil {
			return err
		}
		if err = u.tx.Delete("watch_kv", key); err != nil {
			return err
		}
		return consumeEnd(ctx, u, e, p, s, at)
	})
}

type turnSignal struct {
	Session host.Session `json:"session"`
	At      string       `json:"at"`
}

func consumeEnd(ctx context.Context, u *engineUnit, e *engine.Engine, p assignmentRef, s host.Session, start string) error {
	key := "turn_end." + s.ID
	raw, ok, err := u.tx.Get("watch_kv", key)
	if err != nil || !ok {
		return err
	}
	var signal turnSignal
	if err = json.Unmarshal(raw, &signal); err != nil {
		return err
	}
	if signal.Session != s {
		return nil
	}
	if signal.At >= start {
		w, err := u.GetWork(p.WorkID)
		if err != nil {
			return err
		}
		if a := latest(w); a != nil && a.ID == p.AssignmentID && a.State == engine.AssignmentRunning && (a.IdleAt == "" || a.IdleAt < a.DeliveredAt) {
			if err = finishTurn(ctx, e, w, ""); err != nil {
				return err
			}
		}
	}
	return u.tx.Delete("watch_kv", key)
}
func finishTurn(ctx context.Context, e *engine.Engine, w *engine.Work, summary string) error {
	a := latest(w)
	if err := e.TurnEnded(ctx, w.ID, a.ID); err != nil {
		return err
	}
	if engine.WorkStage(*w).Stage == "person" || a.Report != nil && a.Report.At >= a.DeliveredAt {
		return nil
	}
	if summary == "" {
		summary = "本轮已结束，请打开宿主会话查看最近回复。"
	}
	_, err := e.Report(ctx, w.ID, a.ID, engine.WorkerReport{State: engine.Waiting, Summary: summary})
	return err
}

// 一拍只读取一次派工清单，每个宿主一次取全量；慢宿主调用全部留在数据库事务之外。
func (x *executor) sample(ctx context.Context, recovering bool) error {
	query := "executorRunning"
	if recovering {
		query = "executorOpen"
	}
	x.mu.Lock()
	before := maps.Clone(x.states)
	x.mu.Unlock()
	ws, err := x.works(ctx, query)
	if err != nil {
		return err
	}
	byHost := map[string][]host.Session{}
	for i := range ws {
		s := sessionOf(&ws[i])
		if s.ID != "" {
			byHost[s.Host] = append(byHost[s.Host], s)
		}
	}
	status := map[host.Session]host.State{}
	for name, sessions := range byHost {
		if d := x.drivers[name]; d != nil && d.Capabilities().SessionStatus {
			for s, st := range d.Statuses(ctx, sessions) {
				status[s] = st
			}
		}
	}
	var titles []*engine.Work
	for i := range ws {
		if err := ctx.Err(); err != nil {
			return err
		}
		w := &ws[i]
		a := latest(w)
		if a == nil {
			continue
		}
		s := sessionOf(w)
		st := status[s]
		if st == "" {
			st = host.UnknownState
		}
		if w.Mandate.Basis.Via == engine.ViaHumanDirect {
			if a.State == engine.AssignmentRunning && s.ID != "" && st == host.Gone {
				if err := x.humanSessionClosed(ctx, w, s); err != nil {
					return err
				}
			}
			continue
		}
		switch a.State {
		case engine.AssignmentPending:
			if recovering {
				err = x.recoverPending(ctx, w, st)
			}
		case engine.AssignmentRunning:
			err = x.observe(ctx, w, before[s], st, recovering)
			if err == nil && (st == host.Idle || st == host.Busy) {
				titles = append(titles, w)
			}
		}
		if err != nil && !errors.Is(err, errStale) {
			return fmt.Errorf("核对 Work %s 的会话: %w", w.ID, err)
		}
	}
	if err := x.syncTitles(ctx, titles); err != nil {
		return err
	}
	if recovering {
		return x.recoverSignals(ctx)
	}
	return nil
}

// 人的会话消失只释放原登记。采样后若已换会话，同一代次也不能清掉新的位置。
func (x *executor) humanSessionClosed(ctx context.Context, observed *engine.Work, session host.Session) error {
	return x.transaction(ctx, func(u *engineUnit, e *engine.Engine) error {
		w, err := u.GetWork(observed.ID)
		if err != nil {
			return err
		}
		a := latest(w)
		if a == nil || a.ID != latest(observed).ID || a.State != engine.AssignmentRunning || w.Mandate.Basis.Via != engine.ViaHumanDirect || sessionOf(w) != session {
			return nil
		}
		_, err = e.SetExecution(ctx, w.ID, a.ID, w.Execution.Directory, nil, false)
		return err
	})
}
func (x *executor) observe(ctx context.Context, w *engine.Work, previous sampleState, st host.State, recovering bool) error {
	s := sessionOf(w)
	a := latest(w)
	apply := func() {
		x.mu.Lock()
		defer x.mu.Unlock()
		if x.states[s] == previous {
			x.states[s] = sampleState{state: st, receipt: previous.receipt}
		}
	}
	if st == host.WaitingPerson {
		st = host.Busy
	}
	if st == host.Busy {
		apply()
		return nil
	}
	idle := st == host.Idle && (previous.state == host.Busy || a.DeliveredAt > a.IdleAt)
	lost := st == host.Gone || recovering && st == host.UnknownState
	if !idle && !lost {
		apply()
		return nil
	}
	var summary string
	if idle && (a.Report == nil || a.Report.At < a.DeliveredAt) && engine.WorkStage(*w).Stage != "person" {
		if d := x.drivers[s.Host]; d != nil && d.Capabilities().LastReply {
			reply, err := d.LastReply(ctx, s)
			if err != nil && !errors.Is(err, host.Unsupported) {
				return err
			}
			summary = reply
		}
	}
	err := x.transaction(ctx, func(u *engineUnit, e *engine.Engine) error {
		x.mu.Lock()
		stale := x.states[s] != previous
		x.mu.Unlock()
		if stale {
			return errStale
		}
		current, err := u.GetWork(w.ID)
		if err != nil {
			return err
		}
		now := latest(current)
		if now == nil || now.ID != a.ID || now.State != engine.AssignmentRunning || current.Delivery != nil || sessionOf(current) != s || now.DeliveredAt != a.DeliveredAt || now.IdleAt != a.IdleAt {
			return errStale
		}
		if !recovering || !lost {
			for _, key := range []string{"launch." + a.ID, "deliver." + a.ID} {
				_, ok, err := u.tx.Get("watch_kv", key)
				if err != nil {
					return err
				}
				if ok {
					return errStale
				}
			}
		}
		if lost {
			return e.AssignmentInterrupted(ctx, w.ID, a.ID)
		}
		return finishTurn(ctx, e, current, summary)
	})
	if err == nil {
		apply()
	}
	return err
}
func (x *executor) recoverPending(ctx context.Context, w *engine.Work, st host.State) error {
	if w.Execution == nil || w.Execution.Target == nil || engine.WorkStage(*w).Stage == "person" {
		return nil
	}
	s := sessionOf(w)
	a := latest(w)
	d := x.drivers[s.Host]
	p := assignmentRef{WorkID: w.ID, AssignmentID: a.ID}
	key := "launch." + a.ID
	return x.transaction(ctx, func(u *engineUnit, e *engine.Engine) error {
		current, err := u.GetWork(w.ID)
		if err != nil {
			return err
		}
		gen := latest(current)
		if gen == nil || gen.State != engine.AssignmentPending || gen.ID != a.ID || sessionOf(current) != s || current.Delivery != nil || engine.WorkStage(*current).Stage == "person" {
			return errStale
		}
		at, err := readStart(u.tx, key)
		if err != nil {
			return err
		}
		if s.ID != "" && !confirmed(w) && (d == nil || !d.Capabilities().CreateSeparatesPrompt || at != "") {
			switch st {
			case host.Gone:
				if _, err = e.SetExecution(ctx, w.ID, a.ID, w.Execution.Directory, nil, false); err != nil {
					return err
				}
			case host.Busy:
				if at == "" {
					at = x.r.clock.Now().UTC().Format(utcMillis)
				}
				if err = e.AssignmentStarted(ctx, w.ID, a.ID, at); err != nil {
					return err
				}
				if err = u.tx.Delete("watch_kv", key); err != nil {
					return err
				}
				u.AfterCommit(func() { x.setState(s, host.Busy) })
				return consumeEnd(ctx, u, e, p, s, at)
			default:
				return e.DeliveryFailed(ctx, w.ID, a.ID, "守护进程在派工确认前重启，请在宿主核对是否已收到首条。")
			}
		}
		ok, err := u.HasEffect("launch", a.ID)
		if err != nil || ok {
			return err
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return err
		}
		return u.tx.Enqueue("launch", a.ID, raw)
	})
}
func (x *executor) recoverSignals(ctx context.Context) error {
	return x.transaction(ctx, func(u *engineUnit, e *engine.Engine) error {
		rows, err := u.tx.Lookup("executorSignals")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var signal turnSignal
			if err := json.Unmarshal(row.Value, &signal); err != nil {
				return err
			}
			ws, err := u.works("sessionWorks", signal.Session.Host, signal.Session.ID)
			if err != nil {
				return err
			}
			if len(ws) > 0 {
				w := &ws[0]
				a := latest(w)
				if a != nil {
					pending, err := pendingRound(u, a.ID)
					if err != nil {
						return err
					}
					if pending || w.Delivery != nil {
						continue
					}
					if a.State == engine.AssignmentRunning {
						if err := consumeEnd(ctx, u, e, assignmentRef{WorkID: w.ID, AssignmentID: a.ID}, signal.Session, a.DeliveredAt); err != nil {
							return err
						}
						continue
					}
				}
			}
			if err := u.tx.Delete("watch_kv", row.Key); err != nil {
				return err
			}
		}
		return nil
	})
}
func pendingRound(u *engineUnit, id string) (bool, error) {
	for _, key := range []string{"launch." + id, "deliver." + id} {
		_, ok, err := u.tx.Get("watch_kv", key)
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}
