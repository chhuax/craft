package engine

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// EventDef 是引擎使用的事件声明；适配层负责从 manifest 转换为领域字段。
type EventDef struct {
	Source       string     `json:"source"`
	Event        string     `json:"event"`
	Label        string     `json:"label,omitempty"`
	Default      Mode       `json:"default"`
	Strength     Strength   `json:"strength"`
	FollowupOnly bool       `json:"followupOnly,omitempty,omitzero"`
	Rule         *EventRule `json:"rule,omitempty"`
	OnUnmatched  string     `json:"onUnmatched,omitempty"`
	FeedTier     string     `json:"feedTier,omitempty"`
	Verb         string     `json:"verb,omitempty"`
	NoTarget     string     `json:"noTarget,omitempty"`
	Collapse     *Collapse  `json:"collapse,omitempty"`
}

// EventRule 由事件声明默认接活说明，其规则标识从事件键派生。
type EventRule struct {
	Name        string `json:"name"`
	Instruction string `json:"instruction"`
}

// Collapse 描述一次投递中可合并的事件组及可选分组字段。
type Collapse struct {
	Group string `json:"group"`
	By    string `json:"by,omitempty"`
}

// Settings 保存调用方提供的策略设置，构造快照后可独立修改。
type Settings struct {
	Paused         bool                     `json:"paused"`
	AllowedSenders []ActorRef               `json:"allowedSenders"`
	Events         map[string]EventOverride `json:"events"`
	Rules          []Rule                   `json:"rules"`
	EvalTimeoutMs  int64                    `json:"evalTimeoutMs,omitempty,omitzero"`
}

// EventOverride 仅允许调整扩展事件的运行模式。
type EventOverride struct {
	Mode Mode `json:"mode"`
}

// Rule 保存研判可命中的接活规则及其执行说明。
type Rule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Instruction string `json:"instruction"`
}

type eventID struct{ source, event string }
type activeEvent struct {
	definition EventDef
	mode       Mode
}

func validMode(mode Mode) bool { return mode == ModeOff || mode == ModeObserve || mode == ModeAuto }

func builtinEvents() []EventDef {
	return []EventDef{
		{Source: "craft", Event: "person.answer", Default: ModeAuto, Strength: Explicit, FollowupOnly: true},
		{Source: "craft", Event: "person.request", Default: ModeAuto, Strength: Explicit},
		{Source: "craft", Event: "person.direct", Default: ModeAuto, Strength: Explicit},
	}
}

func (d EventDef) snapshot() EventDef {
	// 声明与返回值都可能被调用方复用，嵌套指针也要与只读索引分离。
	if d.Rule != nil {
		rule := *d.Rule
		d.Rule = &rule
	}
	if d.Collapse != nil {
		collapse := *d.Collapse
		d.Collapse = &collapse
	}
	return d
}

var ruleIDChars = strings.NewReplacer(":", "__", ".", "_")

func buildCatalog(defs []EventDef, overrides map[string]EventOverride) (map[eventID]*activeEvent, map[string]Mode, []Rule, error) {
	builtins := builtinEvents()
	entries := append(builtins, defs...)
	events := make(map[eventID]*activeEvent, len(entries))
	modes := make(map[string]Mode, len(entries))
	defaults := make(map[string]Rule)
	for i, d := range entries {
		key := d.Source + ":" + d.Event
		if d.Source == "" || d.Event == "" {
			return nil, nil, nil, fmt.Errorf("事件键缺少来源或类型 %q", key)
		}
		if _, ok := modes[key]; ok {
			return nil, nil, nil, fmt.Errorf("事件键重复 %q", key)
		}
		if d.Default == "" {
			d.Default = ModeObserve
		}
		if d.Strength == "" {
			d.Strength = Inferred
		}
		if d.OnUnmatched == "" {
			d.OnUnmatched = "filter"
		}
		if !validMode(d.Default) || (d.Strength != Explicit && d.Strength != Inferred) {
			return nil, nil, nil, fmt.Errorf("事件 %q 的默认模式或强度无效", key)
		}
		mode := d.Default
		if override, ok := overrides[key]; ok && i >= len(builtins) {
			if !validMode(override.Mode) {
				return nil, nil, nil, fmt.Errorf("事件 %q 的覆盖模式无效 %q", key, override.Mode)
			}
			mode = override.Mode
		}
		events[eventID{d.Source, d.Event}] = &activeEvent{d.snapshot(), mode}
		modes[key] = mode
		if r := d.Rule; r != nil {
			defaults[key] = Rule{ID: ruleIDChars.Replace(key), Name: r.Name, Enabled: true, Instruction: r.Instruction}
		}
	}
	ordered := make([]Rule, 0, len(defaults))
	for _, key := range slices.Sorted(maps.Keys(defaults)) {
		ordered = append(ordered, defaults[key])
	}
	return events, modes, ordered, nil
}

func mergeRules(defaults, user []Rule) ([]Rule, map[string]Rule, error) {
	merged := make([]Rule, len(defaults), len(defaults)+len(user))
	copy(merged, defaults)
	positions := make(map[string]int, len(defaults))
	for i, r := range merged {
		if _, ok := positions[r.ID]; ok {
			return nil, nil, fmt.Errorf("事件默认规则标识冲突 %q", r.ID)
		}
		positions[r.ID] = i
	}
	byID := make(map[string]Rule, len(defaults)+len(user))
	for _, r := range user {
		if r.ID == "" {
			return nil, nil, fmt.Errorf("用户规则缺少标识")
		}
		if _, ok := byID[r.ID]; ok {
			return nil, nil, fmt.Errorf("用户规则标识重复 %q", r.ID)
		}
		byID[r.ID] = r
		if i, ok := positions[r.ID]; ok {
			merged[i] = r
		} else {
			merged = append(merged, r)
		}
	}
	for _, r := range merged {
		byID[r.ID] = r
	}
	return merged, byID, nil
}
