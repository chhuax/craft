package engine

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// Outcome 指定事件和研判结论。提供 EvalID 时只接受该代次的回执；省略时允许按事件直接决定。
type Outcome struct {
	Kind      DecisionKind               `json:"kind"`
	TriggerID string                     `json:"triggerId"`
	EvalID    string                     `json:"evalId,omitempty"`
	Reason    Reason                     `json:"reason"`
	Audit     map[string]json.RawMessage `json:"audit,omitempty"`
	Scope     string                     `json:"scope,omitempty"`
	Rules     []string                   `json:"rules,omitempty"`
	WorkID    string                     `json:"workId,omitempty"`
}

// AssessByRules 从引擎给出的候选快照生成 v1 回执；推断候选只记录建议，明确授权按当前策略处理。
func AssessByRules(c Candidate, p Policy) Outcome {
	out := Outcome{Kind: Ignore, EvalID: c.EvalID, Reason: Reason{Code: ReasonNoAdmissible}}
	ev := c.Thread.Evaluating
	if ev == nil || len(ev.TriggerIDs) == 0 {
		return out
	}
	out.TriggerID = primaryID(ev)
	var text string
	for i := range c.Triggers {
		if c.Triggers[i].ID == out.TriggerID {
			text = c.Triggers[i].Text
			break
		}
	}
	mode := p.ModeOf(ev.Basis.Source, ev.Basis.Event)
	if mode == ModeOff || mode == "" {
		out.Reason.Code = ReasonModeOff
		return out
	}
	title := assessmentTitle(&c.Thread, text)
	out.Scope = title
	out.Reason = Reason{Code: ReasonSaid, TriggerID: out.TriggerID, Values: map[string]string{"text": "依据事件声明与当前策略研判"}}
	// 明确事件的默认说明可随授权交付；推断事件不靠已启用规则自动升级。
	if d := p.events[eventID{ev.Basis.Source, ev.Basis.Event}]; d != nil && d.definition.Rule != nil {
		id := ruleIDChars.Replace(ev.Basis.Source + ":" + ev.Basis.Event)
		if p.rulesByID[id].Enabled {
			out.Rules = []string{id}
		}
	}
	if ev.Strength == Explicit && p.Admits(*ev, out.Rules) == AdmitOK {
		out.Kind = Accept
		return out
	}
	return observed(out, title, c.Thread.Key)
}

func primaryID(ev *Evaluation) string {
	if ev.Primary != "" {
		return ev.Primary
	}
	if len(ev.TriggerIDs) > 0 {
		return ev.TriggerIDs[len(ev.TriggerIDs)-1]
	}
	return ""
}

func assessmentTitle(th *Thread, text string) string {
	id := strings.TrimSpace(th.Ref.ID())
	var fact string
	// 约定事实可以是非字符串；此时继续使用对象标签或事件摘要。
	if v := th.Meta.Facts[FactTitle].Value; len(v) > 0 && json.Unmarshal(v, &fact) != nil {
		fact = ""
	}
	for _, s := range []string{fact, th.Meta.Label} {
		if s = strings.TrimSpace(s); s != "" && s != id {
			return clipChars(strings.TrimSpace(id+" "+s), 200)
		}
	}
	detail := cmp.Or(clipChars(strings.TrimSpace(text), 60), "未命名")
	return clipChars(strings.TrimSpace(id+" "+detail), 200)
}

func observed(out Outcome, title, key string) Outcome {
	prop := struct {
		Title  string   `json:"title"`
		Scope  string   `json:"scope"`
		Rules  []string `json:"rules"`
		Target string   `json:"target"`
	}{title, cmp.Or(out.Scope, title), append([]string{}, out.Rules...), key}
	// 固定投影只含字符串与字符串数组，标准 JSON 编码没有失败路径。
	raw, _ := json.Marshal(prop)
	out.Audit = maps.Clone(out.Audit)
	if out.Audit == nil {
		out.Audit = make(map[string]json.RawMessage)
	}
	out.Audit["would_accept"] = json.RawMessage("true")
	out.Audit["proposal"] = raw
	out.Kind = Ignore
	text := cmp.Or(out.Reason.Values["text"], out.Reason.Code)
	out.Reason = Reason{Code: ReasonObserved, Values: map[string]string{"text": text}}
	return out
}

func outcomeReasons(out Outcome, ev *Evaluation) []Reason {
	var reasons []Reason
	if out.Kind == Ignore && ev.Strength != Explicit && len(out.Audit) == 0 {
		reasons = append(reasons, Reason{Code: ReasonIgnoredFromCard})
	}
	if out.Reason.Code != "" {
		reasons = append(reasons, out.Reason)
	}
	return reasons
}

func mandateOf(ev *Evaluation, out Outcome) Mandate {
	basis := ev.Basis
	if out.Rules != nil {
		basis.Rules = slices.Clone(out.Rules)
	}
	return Mandate{PolicyVersion: ev.PolicyVersion, Strength: ev.Strength, Actor: ev.Actor, Basis: basis, Scope: out.Scope}
}
