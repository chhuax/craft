package app

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

type inputBatch struct {
	inputs   []engine.WorkInput
	triggers map[string]*engine.Trigger
	ids      []string
	due      time.Time
	person   bool
}

func immediate(w *engine.Work, in engine.WorkInput, ts map[string]*engine.Trigger) bool {
	if in.Kind == "text" {
		// 恢复说明也是 text，来源可由引擎分配的输入 ID 核实，不能借用人的额外槽位。
		return !slices.ContainsFunc(w.Assignments, func(a engine.Assignment) bool { return in.ID == engine.Key("input", w.ID, "restart", a.ID) })
	}
	t := ts[in.TriggerID]
	return in.Kind == "trigger" && t != nil && t.Source == "craft"
}
func (x *executor) batch(ctx context.Context, w *engine.Work) (inputBatch, error) {
	b := inputBatch{}
	byID := make(map[string]engine.WorkInput, len(w.Inputs))
	for _, in := range w.Inputs {
		byID[in.ID] = in
	}
	var triggerIDs []string
	for _, id := range w.Undelivered {
		in, ok := byID[id]
		if !ok {
			return b, fmt.Errorf("队首输入 %q 缺失", id)
		}
		if in.Kind == "trigger" {
			triggerIDs = append(triggerIDs, in.TriggerID)
		}
	}
	err := x.r.repo.Read(ctx, func(r engine.Reader) error { var err error; b.triggers, err = r.GetTriggers(triggerIDs...); return err })
	if err != nil {
		return b, err
	}
	ids := w.DeliveryBatch
	if len(ids) == 0 {
		ids = w.Undelivered[:1]
		first := byID[ids[0]]
		if x.window > 0 && first.Kind == "trigger" && !immediate(w, first, b.triggers) {
			var deadline time.Time
			for i, id := range w.Undelivered {
				in := byID[id]
				if immediate(w, in, b.triggers) {
					ids = w.Undelivered[:i+1]
					b.due = time.Time{}
					break
				}
				if in.Kind != "trigger" {
					break
				}
				at, err := time.Parse(time.RFC3339Nano, in.At)
				if err != nil {
					return b, fmt.Errorf("输入 %q 的入队时间: %w", id, err)
				}
				if i == 0 {
					deadline = at.Add(max(x.maxWait, x.window))
				} else if at.After(b.due) {
					break
				}
				b.due = at.Add(x.window)
				if deadline.Before(b.due) {
					b.due = deadline
				}
				ids = w.Undelivered[:i+1]
			}
			if slices.ContainsFunc(w.Undelivered, func(id string) bool { return immediate(w, byID[id], b.triggers) }) {
				b.due = time.Time{}
			}
		}
	}
	b.ids = ids
	for _, id := range ids {
		in := byID[id]
		b.inputs = append(b.inputs, in)
		b.person = b.person || immediate(w, in, b.triggers)
	}
	return b, nil
}

type collapseKey struct {
	source, group, thread, value string
	context                      bool
}

func collapseOf(in engine.WorkInput, t *engine.Trigger, p *engine.Policy) collapseKey {
	if in.Kind != "trigger" || t == nil {
		return collapseKey{}
	}
	d, ok := p.Event(t.Source, t.Event)
	if !ok || d.Collapse == nil || d.Collapse.Group == "" {
		return collapseKey{}
	}
	k := collapseKey{source: t.Source, group: d.Collapse.Group, context: in.Context}
	if len(t.Threads) > 0 {
		k.thread = engine.ThreadKey(t.Threads[0])
	}
	if by := d.Collapse.By; by != "" {
		val, ok := t.Meta[by]
		if !ok {
			return collapseKey{}
		}
		k.value = string(val)
	}
	return k
}
func (b inputBatch) message(p *engine.Policy) string {
	slices.SortStableFunc(b.inputs, func(a, b engine.WorkInput) int { return cmp.Compare(a.At, b.At) })
	count, last := map[collapseKey]int{}, map[collapseKey]int{}
	keys := make([]collapseKey, len(b.inputs))
	for i, in := range b.inputs {
		k := collapseOf(in, b.triggers[in.TriggerID], p)
		keys[i] = k
		last[k] = i
		count[k]++
	}
	var parts []string
	for i, in := range b.inputs {
		k := keys[i]
		if k.group != "" && last[k] != i {
			continue
		}
		part := inputMessage(in, b.triggers[in.TriggerID])
		if k.group != "" && count[k] > 1 {
			part += fmt.Sprintf("\n合并了 %d 条同类事件。", count[k])
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "\n\n")
}
func (x *executor) deliver(ctx context.Context, workID string) EffectResult {
	for {
		w, err := x.work(ctx, workID)
		if err != nil {
			return effectOutcome(err)
		}
		a := latest(w)
		if a == nil || a.State != engine.AssignmentRunning || w.Delivery != nil || len(w.Undelivered) == 0 || w.Mandate.Basis.Via == engine.ViaHumanDirect {
			return EffectResult{}
		}
		p := assignmentRef{WorkID: workID, AssignmentID: a.ID}
		if _, err = x.current(ctx, p); err != nil {
			return effectOutcome(err)
		}
		b, err := x.batch(ctx, w)
		if err != nil {
			return effectOutcome(err)
		}
		if b.due.After(x.r.clock.Now()) {
			return EffectResult{Due: b.due}
		}
		if engine.WorkStage(*w).Stage == "person" && !b.person && (w.Attention != nil || a.Report != nil && (a.Report.State == engine.Blocked || a.Report.State == engine.Failed)) {
			return EffectResult{Later: true}
		}
		if err = x.deliverBatch(ctx, w, p, b); err != nil {
			return effectOutcome(err)
		}
	}
}
func (x *executor) deliverBatch(ctx context.Context, w *engine.Work, p assignmentRef, b inputBatch) error {
	if w.Execution == nil || w.Execution.Directory == "" && w.Execution.Target == nil {
		return x.r.Commands.DeliveryFailed(ctx, w.ID, p.AssignmentID, "派工缺少执行目录，请核对目标。")
	}
	s := sessionOf(w)
	d := x.drivers[cmp.Or(s.Host, x.name)]
	if d == nil {
		return host.Later
	}
	state := host.Gone
	if s.ID != "" {
		state = d.Status(ctx, s)
	}
	// 空闲状态查不到时仍须占槽。已确认的会话可由驱动在 Append 中续接并核对写入权。
	if state == host.UnknownState && !confirmed(w) {
		return host.Later
	}
	if state == host.Busy && !d.Capabilities().BusyAppend {
		return host.Later
	}
	x.mu.Lock()
	counted := occupiesSlot(w, x.states[s].state)
	x.mu.Unlock()
	if state != host.Busy || !counted {
		if err := x.reserve(ctx, w.ID, b.person); err != nil {
			return err
		}
		defer x.free(w.ID)
	}
	if state == host.Gone || !confirmed(w) {
		if w.Execution.Target == nil {
			return x.r.Commands.DeliveryFailed(ctx, w.ID, p.AssignmentID, "重建会话前需要核对目标目录。")
		}
		if state == host.Gone {
			if err := x.register(ctx, p, w.Execution.Directory, nil, false); err != nil {
				return err
			}
			w.Execution.Session = nil
			w.Execution.Bootstrapped = nil
		}
		if err := x.dispatchAssignment(ctx, w, p); err != nil {
			if errors.Is(err, errTransient) {
				return host.Later
			}
			if _, ok := errors.AsType[*launchFailure](err); ok {
				return errors.Join(err, x.r.Commands.DeliveryFailed(ctx, w.ID, p.AssignmentID, err.Error()))
			}
			return err
		}
		var err error
		w, err = x.current(ctx, p)
		if err != nil {
			return err
		}
		s = sessionOf(w)
		if !d.Capabilities().BusyAppend && d.Status(ctx, s) == host.Busy {
			return host.Later
		}
	}
	mark, err := x.r.Commands.PrepareDelivery(ctx, w.ID, p.AssignmentID, b.ids)
	if err != nil {
		return err
	}
	if mark == "" {
		return errStale
	}
	key := "deliver." + p.AssignmentID
	if _, err = x.startAt(ctx, key); err != nil {
		return err
	}
	current, err := x.current(ctx, p)
	if err != nil {
		return err
	}
	if sessionOf(current) != s || !slices.Equal(current.DeliveryBatch, b.ids) {
		return errStale
	}
	res := d.Append(ctx, s, b.message(x.r.policy.Load()), mark)
	if err := x.appendResult(ctx, p, s, mark, res); err != nil {
		return err
	}
	return x.receipt(context.WithoutCancel(ctx), p, s, key, b.ids)
}

type roundStart struct {
	At string `json:"at"`
}

func readStart(r *store.SQLiteTx, key string) (string, error) {
	raw, ok, err := r.Get("watch_kv", key)
	if err != nil || !ok {
		return "", err
	}
	var start roundStart
	if err = json.Unmarshal(raw, &start); err != nil || start.At == "" {
		return "", fmt.Errorf("无效的轮次起点 %s: %w", key, errors.Join(err, errors.New("需要 at")))
	}
	return start.At, nil
}
