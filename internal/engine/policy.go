package engine

import (
	"cmp"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
)

type actorID struct{ source, id string }

// Policy 是构造后可并发读取的快照；版本、事件及规则索引只在 NewPolicy 中生成。
type Policy struct {
	version       string
	paused        bool
	evalTimeoutMs int64
	events        map[eventID]*activeEvent
	senders       map[actorID]string
	rules         []Rule
	rulesByID     map[string]Rule
}

// NewPolicy 合并扩展声明、内置事件和用户设置，并断开与输入容器的所有可变引用。
func NewPolicy(s Settings, defs []EventDef) (Policy, error) {
	events, modes, defaults, err := buildCatalog(defs, s.Events)
	if err != nil {
		return Policy{}, err
	}
	rules, byID, err := mergeRules(defaults, s.Rules)
	if err != nil {
		return Policy{}, err
	}
	senders := make(map[actorID]string, len(s.AllowedSenders))
	for _, actor := range s.AllowedSenders {
		key := actorID{actor.Source, actor.ID}
		if actor.Source == "" || actor.ID == "" {
			return Policy{}, fmt.Errorf("放行发送人缺少来源或标识")
		}

		// 名单命中靠来源和标识，授权依据只记录设置里提供的显示名。
		senders[key] = actor.Name
	}
	version, err := policyVersion(s.AllowedSenders, modes, rules)
	if err != nil {
		return Policy{}, err
	}
	return Policy{
		version: version, paused: s.Paused, evalTimeoutMs: s.EvalTimeoutMs,
		events: events, senders: senders, rules: rules, rulesByID: byID,
	}, nil
}

func policyVersion(senders []ActorRef, modes map[string]string, rules []Rule) (string, error) {
	sorted := slices.Clone(senders)
	slices.SortFunc(sorted, func(a, b ActorRef) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.ID, b.ID), cmp.Compare(a.Name, b.Name))
	})
	// 单独投影版本输入；运行参数和事件的展示字段不参与研判重排。
	payload := struct {
		AllowedSenders []ActorRef        `json:"allowedSenders"`
		Events         map[string]string `json:"events"`
		Rules          []Rule            `json:"rules"`
	}{sorted, modes, rules}
	raw, err := jsonv2.Marshal(&payload)
	if err != nil {
		return "", fmt.Errorf("编码策略版本: %w", err)
	}
	canonical := jsontext.Value(raw)
	if err := canonical.Canonicalize(); err != nil {
		return "", fmt.Errorf("规范化策略版本: %w", err)
	}
	return Key("policy-v1", string(canonical)), nil
}

// Version 返回已经计算的确定性版本，可用于每次决策的常数时间比较。
func (p Policy) Version() string { return p.version }

// Paused 供调用方在挑选 pending 批次时暂缓自动事件。
func (p Policy) Paused() bool { return p.paused }

// EvalTimeoutMs 返回该快照的研判时限，由运行层使用。
func (p Policy) EvalTimeoutMs() int64 { return p.evalTimeoutMs }

// EffectiveRules 返回按研判顺序排列的规则副本，调用方可独立持有。
func (p Policy) EffectiveRules() []Rule { return slices.Clone(p.rules) }

// Event 查询已登记的事件声明，返回的扩展字段可独立持有。
func (p Policy) Event(source, event string) (EventDef, bool) {
	e, ok := p.events[eventID{source, event}]
	if !ok {
		return EventDef{}, false
	}
	return e.definition.snapshot(), true
}

// ModeOf 读取合并后的模式，空串表示事件未登记。
func (p Policy) ModeOf(source, event string) string {
	if e := p.events[eventID{source, event}]; e != nil {
		return e.mode
	}
	return ""
}

// GateResult 保存准入依据或拒绝码，只有准入成功才有强度与依据。
type GateResult struct {
	Admissible bool
	Strength   Strength
	Basis      MandateBasis
	Reason     string
}

// Gate 按准入优先级检查单条事件；works 是调用方已加载的该对象关联工作。
func (p Policy) Gate(t Trigger, th Thread, works []Work) GateResult {
	e, ok := p.events[eventID{t.Source, t.Event}]
	if !ok {
		return GateResult{Reason: ReasonUnknownEvent}
	}
	d := &e.definition
	if e.mode == "off" {
		if d.FollowupOnly {
			return GateResult{Reason: ReasonFollowupOnly}
		}
		return GateResult{Reason: ReasonModeOff}
	}
	sender, allowed := p.senders[actorID{t.Actor.Source, t.Actor.ID}]
	basis := MandateBasis{Source: t.Source, Event: t.Event, Sender: sender, Mode: e.mode}
	strength := Inferred
	switch {
	case t.Human && t.Addressed != "" && d.Strength == Explicit:
		basis.Via = "addressed"
		strength = Explicit
	case !t.Human && d.Strength == Explicit && !d.FollowupOnly:
		basis.Via = "event"
		strength = Explicit
	case (t.Addressed != "direct" || allowed) && participating(&th, works):
		basis.Via = "followup"
		basis.Mode = ""
		if t.Human && allowed {
			strength = Explicit
		}
	case !d.FollowupOnly:
		basis.Via = "event"
	default:
		return GateResult{Reason: ReasonFollowupOnly}
	}
	return GateResult{Admissible: true, Strength: strength, Basis: basis}
}

func participating(th *Thread, works []Work) bool {
	if len(th.Subscriptions) > 0 {
		return true
	}
	for i := range works {
		if !slices.Contains(th.Context, works[i].ID) {
			return true
		}
	}
	return false
}

// Admits 在 accept 落地前读取当前模式和生效规则，返回 ok、observe 或 refuse。
func (p Policy) Admits(e Evaluation, rules []string) string {
	mode := p.ModeOf(e.Basis.Source, e.Basis.Event)
	if mode == "" || mode == "off" {
		return "refuse"
	}
	if e.Strength == Explicit {
		if e.Basis.Via == "followup" || mode == "auto" {
			return "ok"
		}
		return "observe"
	}
	if mode == "observe" {
		return "observe"
	}
	if len(rules) != 0 && p.rulesEnabled(rules) {
		return "ok"
	}
	return "refuse"
}

func (p Policy) rulesEnabled(ids []string) bool {
	for _, id := range ids {
		if !p.rulesByID[id].Enabled {
			return false
		}
	}
	return true
}

func (p Policy) senderValid(m *Mandate) bool {
	if m.Basis.Sender == "" || m.Basis.Via == "addressed" {
		return true
	}
	_, ok := p.senders[actorID{m.Actor.Source, m.Actor.ID}]
	return ok
}

// Revalidate 在恢复或追加到已结束工作前，核对原授权所依赖的条件是否仍成立。
func (p Policy) Revalidate(m Mandate) bool {
	mode := p.ModeOf(m.Basis.Source, m.Basis.Event)
	if mode == "" || mode == "off" || !p.senderValid(&m) {
		return false
	}
	return m.Strength == Explicit || (mode == "auto" && p.rulesEnabled(m.Basis.Rules))
}
