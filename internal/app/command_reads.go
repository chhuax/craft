package app

import (
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/outbound"
)

// Status 是同一快照内的精确计数，包含已结束的对象。
type Status struct {
	Threads   int `json:"threads"`
	Works     int `json:"works"`
	Attention int `json:"attention"`
}

// Status 只读取事务计数和可能产生待办的活跃集。
func (r CommandReader) Status() (Status, error) {
	items, err := r.Attention()
	if err != nil {
		return Status{}, err
	}
	return r.Counts(len(items))
}

// Counts 合并同一快照已算出的待办数与库内计数，避免分页前重复推导待办。
func (r CommandReader) Counts(attention int) (Status, error) {
	s := Status{Attention: attention}
	rows, err := r.lookup("totals")
	if err != nil {
		return s, err
	}
	for _, row := range rows {
		dst := &s.Works
		if row.Key == "watch_thread" {
			dst = &s.Threads
		}
		if err := json.Unmarshal(row.Value, dst); err != nil {
			return s, fmt.Errorf("读取 %s 总数: %w", row.Key, err)
		}
	}
	return s, nil
}

// Attention 先选有可能产生待办的 Work 和 Thread，再让引擎从完整的相关历史判断取代关系。
func (r CommandReader) Attention() ([]engine.AttentionItem, error) {
	if r.read == nil {
		return nil, nil
	}
	reader := engineReader{*r.read}
	works, err := reader.works("attentionWorks")
	if err != nil {
		return nil, err
	}
	set := engine.AttentionSet{Works: works}
	keys, err := reader.keys("warningKeys")
	if err != nil {
		return nil, err
	}
	threads, err := reader.GetThreads(keys...)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		t := threads[key]
		if t == nil || t.Closed != nil {
			continue
		}
		history, err := reader.DecisionsByThread(key)
		if err != nil {
			return nil, err
		}
		set.Threads = append(set.Threads, *t)
		set.Decisions = append(set.Decisions, history...)
	}
	rows, err := outboxRecords(*r.read, outbound.Pending)
	if err != nil {
		return nil, err
	}
	items := engine.Attention(set)
	failures, err := r.replyFailures()
	if err != nil {
		return nil, err
	}
	if len(failures) == 0 && len(rows) == 0 {
		return items, nil
	}
	return mergeAttention(append(items, failures...), outbound.PendingConfirmations(rows)), nil
}

// SessionAttention 只在已有快照上取开场摘要，区分未初始化与确有零项待办。
func (r CommandReader) SessionAttention() (int, []engine.AttentionItem, error) {
	if r.read == nil {
		return 0, nil, os.ErrNotExist
	}
	items, err := r.Attention()
	if err != nil {
		return 0, nil, err
	}
	return len(items), items[:min(3, len(items))], nil
}

func mergeAttention(items []engine.AttentionItem, pending []outbound.Confirmation) []engine.AttentionItem {
	items = slices.Grow(items, len(pending))
	for _, p := range pending {
		items = append(items, engine.AttentionItem{ID: engine.Key("attention", "confirm", p.ID), Kind: "confirm", Severity: "action",
			Ref: engine.AttentionRef{Attention: p.ID}, Title: p.Action, Detail: p.Preview, Actions: []string{"confirm", "withdraw"}, At: p.At.UTC().Format(utcMillis)})
	}
	slices.SortStableFunc(items, func(a, b engine.AttentionItem) int {
		return cmp.Or(cmp.Compare(a.Severity, b.Severity), cmp.Compare(b.At, a.At), cmp.Compare(a.ID, b.ID))
	})
	return items
}

// WorkPage 按键升序取一页，按对象筛选时合并起源和现有订阅。
func (r CommandReader) WorkPage(open bool, thread, after string, limit int) ([]engine.Work, error) {
	if r.read == nil || limit <= 0 {
		return nil, nil
	}
	reader := engineReader{*r.read}
	name := "workPage"
	if thread != "" {
		name = "threadPage"
		if open {
			name = "openThreadPage"
		}
		return reader.works(name, thread, after, limit)
	}
	if open {
		name = "openPage"
	}
	return reader.works(name, after, limit)
}

// Decisions 返回按标识排序的决策，游标是上一页最后一个标识，对应行可在两页之间被清理。
func (r CommandReader) Decisions(kind, source string, observed bool, after string, limit int) ([]engine.Decision, error) {
	if limit <= 0 {
		return nil, nil
	}
	name := "decisionPage"
	args := []any{after, limit}
	switch {
	case observed:
		name = "observedDecisions"
		args = []any{after, kind, kind, source, source, limit}
	case source != "":
		name = "decisionBySource"
		args = []any{source, after, kind, kind, limit}
	case kind != "":
		name = "decisionByKind"
		args = []any{kind, after, limit}
	}
	rows, err := r.lookup(name, args...)
	if err != nil {
		return nil, err
	}
	decisions := make([]engine.Decision, len(rows))
	err = decodeDocs(rows, func(i int) any { return &decisions[i] })
	return decisions, err
}

// Events 以严格大于 since 的修订号从小到大补拉。
func (r CommandReader) Events(since int64, limit int) ([]engine.Event, error) {
	if r.read == nil {
		return nil, nil
	}
	return (engineReader{*r.read}).EventsSince(since, limit)
}

// ErrOtherWork 表示已验证的执行方试图读取另一个 Work。
var ErrOtherWork = errors.New("执行方只能读取自己的 Work")

// WorkFacts 重读绑定的工作，只按起源及该工作输入中的标识加载一页事件。
func (r CommandReader) WorkFacts(b SessionBinding, id string, offset, limit int) (*engine.Work, []*engine.Trigger, int, error) {
	if offset < 0 || limit <= 0 {
		return nil, nil, 0, fmt.Errorf("无效的工作事实分页范围")
	}
	w, err := r.boundWork(b)
	if err != nil {
		return nil, nil, 0, err
	}
	if w.ID != id {
		return nil, nil, 0, ErrOtherWork
	}
	ids := slices.Clone(w.Origin.TriggerIDs)
	ids = append(ids, w.Origin.TriggerID)
	for _, in := range w.Inputs {
		if in.Kind == "trigger" || in.Kind == "resumed" {
			ids = append(ids, in.TriggerID)
		}
	}
	seen := map[string]bool{"": true}
	ids = slices.DeleteFunc(ids, func(id string) bool {
		duplicate := seen[id]
		seen[id] = true
		return duplicate
	})
	total := len(ids)
	start := min(offset, total)
	ids = ids[start : start+min(limit, total-start)]
	found, err := (engineReader{*r.read}).GetTriggers(ids...)
	if err != nil {
		return nil, nil, 0, err
	}
	triggers := make([]*engine.Trigger, 0, len(ids))
	for _, id := range ids {
		if tr := found[id]; tr != nil {
			triggers = append(triggers, tr)
		}
	}
	return w, triggers, total, nil
}

func (r CommandReader) replyFailures() ([]engine.AttentionItem, error) {
	rows, err := r.lookup("replyFailures")
	if err != nil {
		return nil, err
	}
	items := make([]engine.AttentionItem, len(rows))
	for i, row := range rows {
		item := &items[i]
		if err := json.Unmarshal(row.Value, item); err != nil {
			return nil, fmt.Errorf("解码回复 %q 的失败记录: %w", row.Key, err)
		}
		item.ID = engine.Key("attention", "health", "reply", row.Key)
		item.Kind, item.Severity, item.Title = "health", "warning", "回复发送失败"
	}
	return items, nil
}
