package app

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
)

// HasEffect 也要看见本事务刚入队的请求，中断恢复才能避免重复派工。
func (r engineReader) HasEffect(kind, key string) (bool, error) {
	return r.read.HasEffect(kind, key)
}

// GetReplyStatus 从独立文档读取回执，使队列消费后仍能识别重放。
func (r engineReader) GetReplyStatus(id string) (engine.ReplyStatus, error) {
	var status engine.ReplyStatus
	err := r.get("watch_reply_status", id, &status)
	if err != nil || status != engine.ReplyConfirm && status != engine.ReplyUnknown && status != engine.ReplyDropped {
		return status, err
	}
	// 交给 outbox 后，人工确认、撤回和跨进程核实都以同一读快照里的最新回执为准。
	var record struct {
		State outbound.State `json:"state"`
	}
	if err := r.get("outbox", id, &record); err != nil {
		return "", err
	}
	switch record.State {
	case outbound.Done:
		return engine.ReplySent, nil
	case outbound.Executing, outbound.Unknown:
		return engine.ReplyUnknown, nil
	case outbound.Withdrawn:
		return engine.ReplyDropped, nil
	case outbound.Failed:
		return engine.ReplyFailed, nil
	}
	return status, nil
}

// PutReplyStatus 在入队的同一事务内登记状态，空串仅用于表示缺失。
func (u *engineUnit) PutReplyStatus(id string, status engine.ReplyStatus) error {
	switch status {
	case engine.ReplyPending, engine.ReplyConfirm, engine.ReplySent, engine.ReplyDropped, engine.ReplyFailed, engine.ReplyUnknown:
		return u.put("watch_reply_status", id, status)
	default:
		return fmt.Errorf("回复 %q 的状态无效 %q", id, status)
	}
}

func (r engineReader) deliveryWindow(workID string, head int, ids []string) (*engine.DeliveryWindow, error) {
	rows, err := r.read.Lookup("deliveryWindow", workID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	var w engine.DeliveryWindow
	if err := json.Unmarshal(rows[0].Value, &w); err != nil {
		return nil, fmt.Errorf("解码 Work %q 的投递窗口: %w", workID, err)
	}
	if head > 0 {
		w.Head, err = r.keys("deliveryHead", workID, head)
		if err != nil {
			return nil, err
		}
	}
	w.Inputs = make(map[string]engine.WorkInput, len(ids))
	if len(ids) == 0 {
		return &w, nil
	}
	keys, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("编码投递输入键: %w", err)
	}
	rows, err = r.read.Lookup("deliveryInputs", workID, string(keys))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		var in engine.WorkInput
		if err := json.Unmarshal(row.Value, &in); err != nil {
			return nil, fmt.Errorf("解码 Work %q 的输入 %q: %w", workID, row.Key, err)
		}
		w.Inputs[row.Key] = in
	}
	return &w, nil
}

// GetDeliveryWindow 按请求数截取队首；已出队的输入仍可按 ID 补记已读。
func (r engineReader) GetDeliveryWindow(workID string, ids []string) (*engine.DeliveryWindow, error) {
	return r.deliveryWindow(workID, len(ids), ids)
}

// PutDeliveryWindow 重读要消费的前缀与当前派工，使旧窗口不能覆盖后来入队的输入。
func (u *engineUnit) PutDeliveryWindow(workID string, d engine.DeliveryWindow, consumed int) error {
	if consumed < 0 || consumed > len(d.Head) {
		return fmt.Errorf("工作 %q 的投递队首已变", workID)
	}
	w, err := u.deliveryWindow(workID, consumed, slices.Collect(maps.Keys(d.Inputs)))
	if err != nil {
		return err
	}
	if w == nil {
		return fmt.Errorf("投递 Work %q 缺失", workID)
	}
	if !slices.Equal(w.Head, d.Head[:consumed]) {
		return fmt.Errorf("工作 %q 的投递队首已变", workID)
	}
	if w.Ended || w.Assignment == nil || d.Assignment == nil || w.Assignment.ID != d.Assignment.ID {
		return fmt.Errorf("工作 %q 的投递派工已变", workID)
	}
	// 调用方可以处理端口错误后继续提交，单个回执也必须能独立回滚。
	return u.tx.Savepoint(u.ctx, func(tx *store.SQLiteTx) error {
		for id, update := range d.Inputs {
			in, ok := w.Inputs[id]
			if !ok || update.ID != id {
				return fmt.Errorf("工作 %q 没有输入 %q", workID, id)
			}
			in.ReadAt, in.DeliveredAt, in.DeliveredTo = update.ReadAt, update.DeliveredAt, update.DeliveredTo
			raw, err := json.Marshal(&in)
			if err != nil {
				return fmt.Errorf("编码输入 %q: %w", id, err)
			}
			if err := tx.Update("deliveryInput", string(raw), workID); err != nil {
				return err
			}
		}
		if consumed > 0 {
			if err := tx.Update("consumeHead", consumed, workID); err != nil {
				return err
			}
		}
		// 合并补丁以 null 清除可选字段，readAt 的显式 null 则保留在输入文档内。
		if len(d.Batch) == 0 {
			d.Batch = nil
		}
		patch, err := json.Marshal(map[string]any{
			"deliveredAt": d.Assignment.DeliveredAt, "updatedAt": d.UpdatedAt,
			"delivery": d.Failure, "deliveryBatch": d.Batch,
		}, json.FormatNilSliceAsNull(true))
		if err != nil {
			return fmt.Errorf("编码投递回执: %w", err)
		}
		return tx.Update("deliveryState", string(patch), workID)
	})
}
