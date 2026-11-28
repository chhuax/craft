package engine

import (
	"reflect"
	"testing"
)

func TestPolicyEffects(t *testing.T) {
	actor := ActorRef{Source: "chat", ID: "u"}
	rule := Rule{ID: "r", Name: "检查", Enabled: true, Instruction: "旧"}
	disabled := rule
	disabled.Enabled = false
	edited := rule
	edited.Instruction = "新"
	makePolicy := func(mode Mode, allowed bool, r *Rule) Policy {
		s := Settings{}
		if allowed {
			s.AllowedSenders = []ActorRef{actor}
		}
		if r != nil {
			s.Rules = []Rule{*r}
		}
		var defs []EventDef
		if mode != "" {
			defs = []EventDef{{Source: "gh", Event: "evt", Default: mode}}
		}
		return buildPolicy(t, s, defs...)
	}
	old := makePolicy("auto", true, &rule)
	no := false
	lost := PolicyEffect{RequesterAllowed: &no, Requester: &actor}
	for _, tc := range []struct {
		name     string
		old, new Policy
		strength Strength
		via      Via
		want     PolicyEffect
	}{
		{"未变化", old, old, Inferred, "event", PolicyEffect{}},
		{"规则被关闭", old, makePolicy("auto", true, &disabled), Inferred, "event", PolicyEffect{Rules: []RuleChange{{ID: "r", Name: "检查", Change: "disabled"}}}},
		{"规则被删除", old, makePolicy("auto", true, nil), Explicit, "event", PolicyEffect{Rules: []RuleChange{{ID: "r", Name: "检查", Change: "disabled"}}}},
		{"规则内容更新", old, makePolicy("auto", true, &edited), Inferred, "event", PolicyEffect{Rules: []RuleChange{{ID: "r", Name: "检查", Change: "changed", Instruction: &edited.Instruction}}}},
		{"离开自动", old, makePolicy("observe", true, &rule), Inferred, "event", PolicyEffect{Proactive: &no}},
		{"明确授权观察模式", old, makePolicy("observe", true, &rule), Explicit, "event", PolicyEffect{}},
		{"发送人移除优先", old, makePolicy("off", false, &rule), Inferred, "followup", lost},
		{"事件从目录移除", old, makePolicy("", true, &rule), Explicit, "event", lost},
		{"定向不依赖名单", old, makePolicy("auto", false, &rule), Explicit, "addressed", PolicyEffect{}},
		{"旧授权已失效", makePolicy("off", true, &rule), makePolicy("off", false, &rule), Inferred, "event", PolicyEffect{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Work{ID: "w", Mandate: Mandate{Strength: tc.strength, Actor: actor, Basis: MandateBasis{Source: "gh", Event: "evt", Sender: "u", Via: tc.via, Rules: []string{"r"}}}}
			got := PolicyEffects([]Work{w}, tc.old, tc.new)
			if !reflect.DeepEqual(got["w"], tc.want) || (len(got) == 0) != reflect.DeepEqual(tc.want, PolicyEffect{}) {
				t.Fatalf("%+v，期望 %+v", got, tc.want)
			}
		})
	}
}

func TestPolicyEffectsUseDefaultRules(t *testing.T) {
	def := EventDef{Source: "gh", Event: "item.changed", Rule: &EventRule{Name: "检查", Instruction: "原说明"}}
	before := buildPolicy(t, Settings{}, def)
	after := buildPolicy(t, Settings{Rules: []Rule{{ID: "gh__item_changed", Name: "检查", Enabled: false}}}, def)
	affected := Work{ID: "a", Mandate: Mandate{Strength: Explicit, Basis: MandateBasis{Source: "gh", Event: "item.changed", Rules: []string{"gh__item_changed"}}}}
	unaffected := affected
	unaffected.ID = "b"
	unaffected.Mandate.Basis.Rules = []string{"other"}
	got := PolicyEffects([]Work{affected, unaffected}, before, after)
	if len(got) != 1 || len(got["a"].Rules) != 1 || got["a"].Rules[0].Change != "disabled" {
		t.Fatal(got)
	}
}
