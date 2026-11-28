package engine

import (
	"cmp"
	"context"
	"fmt"
	"slices"
)

// PolicyChanged 在应用层保存生效设置的同一个写事务内运行。调用方须把本命令及
// 设置写入的任何错误传回外层事务；在此另开 Store.Atomic 会与外层争用写锁。
func (e *Engine) PolicyChanged(ctx context.Context, old, next Policy, u Unit) error {
	_ = e.cfg.Policy()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("改版前检查事务: %w", err)
	}
	if old.Version() == next.Version() {
		return nil
	}
	now := e.cfg.Clock()
	// 只有罕见的策略改版需要比较每份在办授权；普通命令仅沿对象订阅寻址。
	works, err := u.OpenWorks()
	if err != nil {
		return fmt.Errorf("读取改版时的在办工作: %w", err)
	}
	effects := PolicyEffects(works, old, next)
	slices.SortStableFunc(works, func(a, b Work) int { return cmp.Compare(a.CreatedAt, b.CreatedAt) })
	for i := range works {
		w := &works[i]
		effect, affected := effects[w.ID]
		if !affected {
			continue
		}
		id := Key("input", w.ID, "policy", old.Version(), next.Version(), now)
		if slices.ContainsFunc(w.Inputs, func(in WorkInput) bool { return in.ID == id }) {
			continue
		}
		w.Inputs = append(w.Inputs, WorkInput{ID: id, Kind: "policy-changed", At: now, From: old.Version(), To: next.Version(), Effect: &effect})
		w.Undelivered = append(w.Undelivered, id)
		w.UpdatedAt = now
		if err := u.PutWork(*w); err != nil {
			return fmt.Errorf("写入 Work %q 改版输入: %w", w.ID, err)
		}
		if err := wake(u, w); err != nil {
			return err
		}
	}
	threads, err := u.EvaluatingThreads()
	if err != nil {
		return fmt.Errorf("读取改版时的研判批次: %w", err)
	}
	for i := range threads {
		th := &threads[i]
		if th.Evaluating.PolicyVersion == next.Version() {
			continue
		}
		if _, err := requeueBatch(u, th, Reason{Code: ReasonPolicyChanged}, "", now); err != nil {
			return fmt.Errorf("重排 Thread %q 的旧版研判: %w", th.Key, err)
		}
	}
	return nil
}
