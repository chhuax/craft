package app

import (
	"context"
	"errors"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

type assessmentHandler struct {
	r     *Runtime
	scope engine.SourceScope
}

// Kind 对应引擎随研判批次提交的 effect。
func (assessmentHandler) Kind() string { return "assessment" }

// Limit 让各批次串行研判。
func (assessmentHandler) Limit() int { return 1 }

// Ordered 允许后续对象在某个批次退避时继续研判。
func (assessmentHandler) Ordered() bool { return false }

// Quiet 让持续失败的研判进入健康告警。
func (assessmentHandler) Quiet() bool { return false }

// 重排会生成新 evalId，连续超时次数归队首事件。计数与引擎回报必须同事务提交。
type assessmentUnit struct {
	engine.Store
	unit engine.Unit
}

// Atomic 在已有事务内开保存点，使回报和超时计数同生共死。
func (u assessmentUnit) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return u.unit.Atomic(ctx, fn)
}

// Run 只接受仍活跃的 evalId，重放已结束代次可安全删除旧 effect。
func (h assessmentHandler) Run(ctx context.Context, row store.Effect) EffectResult {
	c, err := h.r.Commands.Candidate(ctx, row.Key)
	if err == nil {
		out := engine.AssessByRules(c, *h.r.policy.Load())
		err = h.r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error { return h.finish(ctx, tx, c, out) })
	}
	if cause, ok := errors.AsType[*engine.UserError](err); ok && cause.Code == engine.CodeEvalFinished {
		return EffectResult{}
	}
	return EffectResult{Err: err}
}

func (h assessmentHandler) finish(ctx context.Context, tx *store.SQLiteTx, c engine.Candidate, out engine.Outcome) error {
	u := h.r.repo.in(ctx, tx)
	eng, err := h.r.newEngine(assessmentUnit{h.r.repo, u}, h.scope)
	if err != nil {
		return err
	}
	ev := c.Thread.Evaluating
	// 即使重排与旧 effect 重放交错，也只有仍持有这个代次的对象能消费重试预算。
	th, err := u.ThreadByEval(ev.ID)
	if err != nil || th == nil {
		return err
	}
	key := "assessment.timeout:" + ev.TriggerIDs[0]
	tx.AfterCommit(h.r.Request)
	expired, err := h.expired(ev)
	if err != nil {
		return err
	}
	if expired {
		count, err := tx.NextCounter(key)
		if err != nil {
			return err
		}
		if count < 3 {
			_, err = eng.ReassessEval(ctx, ev.ID)
			return err
		}
		out = engine.Outcome{EvalID: ev.ID, TriggerID: out.TriggerID, Kind: engine.Unavailable, Reason: engine.Reason{Code: engine.ReasonTimeout}}
	}
	_, err = eng.Decide(ctx, out)
	if refusal, ok := errors.AsType[*engine.UserError](err); ok && refusal.Code == engine.CodeWorkerDecides {
		// 执行方已认领时由下一轮 Pass 重查分发处置，释放本代次，避免卡住同对象后来的事件。
		_, err = eng.ReassessEval(ctx, ev.ID)
	}
	if err != nil {
		return err
	}
	return tx.Delete("watch_kv", key)
}

func (h assessmentHandler) expired(ev *engine.Evaluation) (bool, error) {
	limit := h.r.policy.Load().EvalTimeoutMs()
	if limit <= 0 {
		return false, nil
	}
	elapsed, err := engine.DiffMs(h.r.clock.Now().UTC().Format(utcMillis), ev.StartedAt)
	return elapsed >= limit, err
}
