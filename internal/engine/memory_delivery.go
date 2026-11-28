package engine

import (
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
)

// 全量写入时一次建立位置索引。后续批量回执共享历史和队尾，仅替换本批输入。
// 顺序独立保存，旧文档即使缺少输入 ID，也仍能按原顺序完整往返。
type memoryInputs struct {
	original  [][]byte
	positions map[string]int
	updates   *indexNode
	queue     []string
}

func indexInputs(inputs []WorkInput, queue []string) (*memoryInputs, error) {
	m := &memoryInputs{queue: slices.Clone(queue)}
	if len(inputs) == 0 {
		return m, nil
	}
	m.original = make([][]byte, len(inputs))
	m.positions = make(map[string]int, len(inputs))
	for i := range inputs {
		data, err := jsonv2.Marshal(&inputs[i])
		if err != nil {
			return nil, fmt.Errorf("编码输入 %q: %w", inputs[i].ID, err)
		}
		m.original[i] = data
		m.positions[inputs[i].ID] = i
	}
	return m, nil
}

func (m *memoryInputs) load(w *Work) error {
	w.Undelivered = append([]string{}, m.queue...)
	w.Inputs = make([]WorkInput, len(m.original))
	for i, raw := range m.original {
		if err := jsonv2.Unmarshal(raw, &w.Inputs[i]); err != nil {
			return fmt.Errorf("读取历史输入: %w", err)
		}
	}
	return m.updates.walk(func(row *memoryRow) error { return row.decode(&w.Inputs[m.positions[row.lookup]]) })
}

func (m *memoryInputs) input(id string, index int) (WorkInput, error) {
	raw := m.original[index]
	if updated := m.updates.get(id); updated != nil {
		raw = updated.data
	}
	var in WorkInput
	if err := jsonv2.Unmarshal(raw, &in); err != nil {
		return WorkInput{}, fmt.Errorf("读取投递输入 %q: %w", id, err)
	}
	return in, nil
}

func (r *memoryView) GetDeliveryWindow(workID string, ids []string) (*DeliveryWindow, error) {
	row := r.works.get(workID)
	if row == nil {
		return nil, nil
	}
	var w Work
	if err := jsonv2.Unmarshal(row.data, &w); err != nil {
		return nil, fmt.Errorf("读取 Work %q 的派工: %w", workID, err)
	}
	m := row.inputs
	d := DeliveryWindow{Ended: w.Ended != nil, Failure: w.Delivery, Batch: w.DeliveryBatch, UpdatedAt: w.UpdatedAt, Head: slices.Clone(m.queue[:min(len(ids), len(m.queue))]), Inputs: make(map[string]WorkInput, len(ids))}
	if n := len(w.Assignments); n > 0 {
		d.Assignment = &w.Assignments[n-1]
	}
	for _, id := range ids {
		i, ok := m.positions[id]
		if !ok {
			continue
		}
		in, err := m.input(id, i)
		if err != nil {
			return nil, err
		}
		d.Inputs[id] = in
	}
	return &d, nil
}

func (u *memoryUnit) PutDeliveryWindow(workID string, d DeliveryWindow, consumed int) error {
	old := u.works.get(workID)
	if old == nil {
		return fmt.Errorf("投递 Work %q 缺失", workID)
	}
	m := *old.inputs
	if consumed < 0 || consumed > len(d.Head) || consumed > len(m.queue) || !slices.Equal(d.Head[:consumed], m.queue[:consumed]) {
		return fmt.Errorf("Work %q 的投递队首已变", workID)
	}
	var w Work
	if err := jsonv2.Unmarshal(old.data, &w); err != nil {
		return fmt.Errorf("读取 Work %q 的派工: %w", workID, err)
	}
	if w.Ended != nil || len(w.Assignments) == 0 || d.Assignment == nil || w.Assignments[len(w.Assignments)-1].ID != d.Assignment.ID {
		return fmt.Errorf("Work %q 的投递派工已变", workID)
	}
	w.Assignments[len(w.Assignments)-1].DeliveredAt = d.Assignment.DeliveredAt
	w.Delivery, w.DeliveryBatch, w.UpdatedAt = d.Failure, d.Batch, d.UpdatedAt
	raw, err := jsonv2.Marshal(&w)
	if err != nil {
		return fmt.Errorf("编码 Work %q 的派工: %w", workID, err)
	}
	// 单次端口调用失败也不应留下部分 input 更新，先完成全部校验与编码。
	changes := make([]*memoryRow, 0, len(d.Inputs))
	for id, in := range d.Inputs {
		i, ok := m.positions[id]
		if !ok || in.ID != id {
			return fmt.Errorf("Work %q 没有输入 %q", workID, id)
		}
		original, err := m.input(id, i)
		if err != nil {
			return err
		}
		original.ReadAt, original.DeliveredAt, original.DeliveredTo = in.ReadAt, in.DeliveredAt, in.DeliveredTo
		data, err := jsonv2.Marshal(&original)
		if err != nil {
			return fmt.Errorf("编码输入 %q: %w", id, err)
		}
		changes = append(changes, &memoryRow{data: data, lookup: id})
	}
	for _, change := range changes {
		m.updates = m.updates.set(u.mark, change.lookup, change)
	}
	m.queue = m.queue[consumed:]
	next := *old
	next.inputs = &m
	next.data = raw
	u.works = u.works.set(u.mark, workID, &next)
	u.open = u.open.set(u.mark, next.order, &next)
	return nil
}
