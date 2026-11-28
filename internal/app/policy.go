package app

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

// 重启时目录中的默认模式和规则可能已更新，仅保存用户的覆盖项无法还原旧授权。
type savedPolicy struct {
	Version  string            `json:"version"`
	Settings engine.Settings   `json:"settings"`
	Catalog  []engine.EventDef `json:"catalog"`
}

// ApplyPolicy 在同一事务内更新受影响的工作、研判和完整设置，失败时可重试。
// 不参与版本计算的运行参数也随成功提交一起发布给后续命令。
func (r *Runtime) ApplyPolicy(ctx context.Context, settings engine.Settings, catalog []engine.EventDef) error {
	next, err := engine.NewPolicy(settings, catalog)
	if err != nil {
		return fmt.Errorf("构造新策略: %w", err)
	}
	const key = "policy.settings"
	err = r.repo.db.WriteAndPublish(ctx, func(tx *store.SQLiteTx) error {
		raw, found, err := tx.Get("watch_kv", key)
		if err != nil {
			return err
		}
		if found {
			var prev savedPolicy
			if err := json.Unmarshal(raw, &prev); err != nil {
				return fmt.Errorf("解码上次策略: %w", err)
			}
			if prev.Version != next.Version() {
				old, err := engine.NewPolicy(prev.Settings, prev.Catalog)
				if err != nil {
					return fmt.Errorf("还原上次策略: %w", err)
				}
				if old.Version() != prev.Version {
					return fmt.Errorf("已保存策略的版本与全文不符")
				}
				if err := r.pass.PolicyChanged(ctx, old, next, r.repo.in(ctx, tx)); err != nil {
					return err
				}
			}
		}
		value, err := json.Marshal(savedPolicy{next.Version(), settings, catalog}, json.Deterministic(true))
		if err != nil {
			return fmt.Errorf("编码待生效策略: %w", err)
		}
		if bytes.Equal(raw, value) {
			return nil
		}
		tx.AfterCommit(r.Request)
		return tx.Put("watch_kv", key, value)
	}, func() { r.policy.Store(&next) })
	if err != nil {
		return fmt.Errorf("应用策略 %s: %w", next.Version(), err)
	}
	return nil
}
