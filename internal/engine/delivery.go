package engine

import (
	"context"
	"fmt"
	"slices"
)

// DeliveryID 把逻辑请求标识变成可安全传给宿主的幂等标记。
func DeliveryID(requestID string) string { return "w" + Key(requestID) }

func batchMarker(ids []string) string {
	if len(ids) == 1 {
		return DeliveryID(ids[0])
	}
	sorted := slices.Clone(ids)
	sortInputIDs(sorted)
	return DeliveryID(Key(append([]string{"delivery-batch"}, sorted...)...))
}

// 普通短批次直接比较。较大批次从末字节起稳定计数排序；输入 ID 是定长摘要，
// 即使积压增长也只需固定趟数。零号桶保留短 ID 在共同前缀之前的字典序。
func sortInputIDs(ids []string) {
	if len(ids) <= 32 {
		slices.Sort(ids)
		return
	}
	width := 0
	for _, id := range ids {
		width = max(width, len(id))
	}
	tmp := make([]string, len(ids))
	bucket := func(id string, pos int) int {
		if pos >= len(id) {
			return 0
		}
		return 1 + int(id[pos])
	}
	for pos := width - 1; pos >= 0; pos-- {
		var offsets [258]int
		for _, id := range ids {
			offsets[bucket(id, pos)+1]++
		}
		for i := 1; i < len(offsets); i++ {
			offsets[i] += offsets[i-1]
		}
		for _, id := range ids {
			b := bucket(id, pos)
			tmp[offsets[b]] = id
			offsets[b]++
		}
		copy(ids, tmp)
	}
}

func (e *Engine) withDelivery(ctx context.Context, workID, assignmentID string, ids []string, fn func(Unit, *DeliveryWindow, string) error) error {
	err := e.cfg.Store.Atomic(ctx, func(u Unit) error {
		_ = e.cfg.Policy()
		w, err := u.GetDeliveryWindow(workID, ids)
		if err != nil {
			return err
		}
		if w == nil {
			return &UserError{Code: CodeUnknownWork}
		}
		if w.Ended || w.Assignment == nil || w.Assignment.ID != assignmentID || w.Assignment.State != AssignmentRunning {
			return nil
		}
		return fn(u, w, e.cfg.Clock())
	})
	if err != nil {
		return fmt.Errorf("投递 Work %q 派工 %q: %w", workID, assignmentID, err)
	}
	return nil
}

func (w *DeliveryWindow) matches(ids []string) bool {
	return len(ids) > 0 && len(w.Inputs) == len(ids) && slices.Equal(ids, w.Head) && (len(w.Batch) == 0 || slices.Equal(ids, w.Batch))
}

// PrepareDelivery 固定本次队首批次。已有批次返回同一标记；已确认、失效或待人工核对时返回空串。
// 单条在途 effect 的发送由运行层串行，确定未发送的 Later 重试仍使用同一标记。
func (e *Engine) PrepareDelivery(ctx context.Context, workID, assignmentID string, ids []string) (string, error) {
	var marker string
	err := e.withDelivery(ctx, workID, assignmentID, ids, func(u Unit, w *DeliveryWindow, _ string) error {
		if w.Failure != nil || !w.matches(ids) {
			return nil
		}
		if len(w.Batch) == 0 {
			w.Batch = ids
			w.Inputs = nil
			if err := u.PutDeliveryWindow(workID, *w, 0); err != nil {
				return err
			}
		}
		marker = batchMarker(ids)
		return nil
	})
	if err != nil {
		return "", err
	}
	return marker, nil
}

// InputsDelivered 仅确认仍在队首的原批次。at 由运行层传入首次尝试的轮次起点，空串取命令时钟。
func (e *Engine) InputsDelivered(ctx context.Context, workID, assignmentID string, ids []string, read bool, at string) error {
	return e.withDelivery(ctx, workID, assignmentID, ids, func(u Unit, w *DeliveryWindow, now string) error {
		if !w.matches(ids) {
			return nil
		}
		at, err := deliveryTime(at, now)
		if err != nil {
			return err
		}
		for id, in := range w.Inputs {
			if in.Kind != "trigger" {
				delete(w.Inputs, id)
				continue
			}
			if in.ReadAt.Time == "" {
				in.ReadAt.Present = true
				if read {
					in.ReadAt.Time = at
				}
			}
			in.DeliveredTo, in.DeliveredAt = assignmentID, at
			w.Inputs[id] = in
		}
		w.Assignment.DeliveredAt = at
		w.Batch = nil
		w.Failure = nil
		w.UpdatedAt = now
		return u.PutDeliveryWindow(workID, *w, len(ids))
	})
}

func deliveryTime(at, now string) (string, error) {
	if at == "" {
		return now, nil
	}
	return IsoTime(at)
}

// InputsRead 记录本派工取走的事件；重复读取保留第一次的时间。
func (e *Engine) InputsRead(ctx context.Context, workID, assignmentID string, ids []string) error {
	return e.withDelivery(ctx, workID, assignmentID, ids, func(u Unit, w *DeliveryWindow, now string) error {
		for id, in := range w.Inputs {
			if in.Kind != "trigger" || in.ReadAt.Time != "" {
				delete(w.Inputs, id)
				continue
			}
			in.ReadAt = ReadTime{Present: true, Time: now}
			w.Inputs[id] = in
		}
		if len(w.Inputs) == 0 {
			return nil
		}
		return u.PutDeliveryWindow(workID, *w, 0)
	})
}
