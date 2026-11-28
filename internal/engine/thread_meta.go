package engine

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// DescribeThread 可在首条事件到达之前描述对象。nil 的 label、url 表示保留原值，
// facts 中缺席的键也保留；只有显式传入的 nil 指针才删除对应事实。
func (e *Engine) DescribeThread(ctx context.Context, ref ThreadRef, label, url *string, facts map[string]*Fact) error {
	key := ThreadKey(ref)
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		th, err := u.GetThread(key)
		if err != nil {
			return err
		}
		now := e.cfg.Clock()
		th = threadOrNew(th, ref, now)
		if label != nil {
			th.Meta.Label = *label
		}
		if url != nil {
			th.Meta.URL = *url
		}
		for id, f := range facts {
			if f == nil {
				delete(th.Meta.Facts, id)
				continue
			}
			if th.Meta.Facts == nil {
				th.Meta.Facts = make(map[string]Fact)
			}
			th.Meta.Facts[id] = *f
		}
		if err := u.PutThread(*th); err != nil {
			return err
		}
		return emitThread(u, "thread.described", key, now)
	})
	if err != nil {
		return fmt.Errorf("描述 Thread %q: %w", key, err)
	}
	return nil
}

// PatchThreadMeta 依对象键合并已知的 label、url；null 删除对应值。
// facts 必须经 DescribeThread 传入，以免误把增量事实解释成整组替换。
func (e *Engine) PatchThreadMeta(ctx context.Context, key string, patch map[string]json.RawMessage) error {
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		if _, ok := patch["facts"]; ok {
			return &UserError{Code: CodeNotAdmissible, Values: map[string]string{"reason": "facts 须通过对象描述修改"}}
		}
		th, err := u.GetThread(key)
		if err != nil || th == nil {
			return err
		}
		changed := false
		for k, raw := range patch {
			var dst *string
			switch k {
			case "label":
				dst = &th.Meta.Label
			case "url":
				dst = &th.Meta.URL
			default:
				continue
			}
			old := *dst
			if err := jsonv2.Unmarshal(raw, dst); err != nil {
				return fmt.Errorf("读取元信息 %q: %w", k, err)
			}
			changed = changed || old != *dst
		}
		if !changed {
			return nil
		}
		if err := u.PutThread(*th); err != nil {
			return err
		}
		return emitThread(u, "thread.patched", key, e.cfg.Clock())
	})
	if err != nil {
		return fmt.Errorf("修改 Thread %q 元信息: %w", key, err)
	}
	return nil
}
