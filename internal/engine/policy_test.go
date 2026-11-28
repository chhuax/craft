package engine

import (
	"reflect"
	"slices"
	"testing"
)

func buildPolicy(t testing.TB, settings Settings, defs ...EventDef) Policy {
	t.Helper()
	p, err := NewPolicy(settings, defs)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGate(t *testing.T) {
	actor := ActorRef{Source: "chat", ID: "u", Name: "小周"}
	for _, tc := range []struct {
		name                        string
		strength                    Strength
		follow, human               bool
		addressed, mode             string
		allowed, sub, work, context bool
		want                        Strength
		via, reason                 string
	}{
		{name: "未知事件", human: true, addressed: "direct", mode: "auto", allowed: true, sub: true, reason: "unknownEvent"},
		{name: "人定向优先", strength: Explicit, follow: true, human: true, addressed: "mention", mode: "observe", sub: true, want: Explicit, via: "addressed"},
		{name: "非人明确入口", strength: Explicit, mode: "observe", allowed: true, sub: true, want: Explicit, via: "event"},
		{name: "非人跟进不能起单", strength: Explicit, follow: true, mode: "auto", reason: "followupOnly"},
		{name: "订阅即参与", strength: Inferred, follow: true, human: true, mode: "observe", allowed: true, sub: true, work: true, context: true, want: Explicit, via: "followup"},
		{name: "仅上下文", strength: Inferred, follow: true, human: true, mode: "auto", allowed: true, work: true, context: true, reason: "followupOnly"},
		{name: "已有非上下文工作", strength: Inferred, follow: true, human: true, mode: "auto", work: true, want: Inferred, via: "followup"},
		{name: "放行机器跟进", strength: Inferred, follow: true, mode: "auto", allowed: true, sub: true, want: Inferred, via: "followup"},
		{name: "陌生人私聊不可跟进", strength: Inferred, follow: true, human: true, addressed: "direct", mode: "auto", sub: true, reason: "followupOnly"},
		{name: "陌生人私聊回落到入口", strength: Inferred, human: true, addressed: "direct", mode: "auto", sub: true, want: Inferred, via: "event"},
		{name: "放行人私聊可跟进", strength: Inferred, follow: true, human: true, addressed: "direct", mode: "auto", allowed: true, sub: true, want: Explicit, via: "followup"},
		{name: "陌生人提及可跟进", strength: Inferred, follow: true, human: true, addressed: "mention", mode: "observe", sub: true, want: Inferred, via: "followup"},
		{name: "普通入口", strength: Inferred, human: true, mode: "auto", want: Inferred, via: "event"},
		{name: "关闭压过明确和放行", strength: Explicit, human: true, addressed: "direct", mode: "off", allowed: true, sub: true, reason: "modeOff"},
		{name: "关闭压过非人入口", strength: Explicit, mode: "off", allowed: true, sub: true, reason: "modeOff"},
		{name: "关闭跟进专用的原因", strength: Explicit, follow: true, human: true, addressed: "mention", mode: "off", allowed: true, sub: true, reason: "followupOnly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := Settings{}
			if tc.allowed {
				settings.AllowedSenders = []ActorRef{actor}
			}
			var defs []EventDef
			if tc.strength != "" {
				defs = []EventDef{{Source: "gh", Event: "update", Default: tc.mode, Strength: tc.strength, FollowupOnly: tc.follow}}
			}
			tr := Trigger{Source: "gh", Event: "update", Actor: actor, Human: tc.human, Addressed: tc.addressed}
			th := Thread{}
			var works []Work
			if tc.sub {
				th.Subscriptions = []string{"w"}
			}
			if tc.context {
				th.Context = []string{"w"}
			}
			if tc.work {
				works = []Work{{ID: "w"}}
			}
			got := buildPolicy(t, settings, defs...).Gate(tr, th, works)
			if got.Admissible != (tc.reason == "") || got.Reason != tc.reason || got.Strength != tc.want || got.Basis.Via != tc.via {
				t.Fatal(got)
			}
			if got.Admissible {
				mode := tc.mode
				if tc.via == "followup" {
					mode = ""
				}
				sender := ""
				if tc.allowed {
					sender = actor.Name
				}
				want := MandateBasis{Via: tc.via, Source: "gh", Event: "update", Mode: mode, Sender: sender}
				if !reflect.DeepEqual(got.Basis, want) {
					t.Fatalf("%+v，期望 %+v", got.Basis, want)
				}
			}
		})
	}
}

func TestBuiltins(t *testing.T) {
	for _, ev := range []string{"person.answer", "person.request", "person.direct"} {
		p := buildPolicy(t, Settings{Paused: true, Events: map[string]EventOverride{"craft:" + ev: {Mode: "off"}}})
		tr := Trigger{Source: "craft", Event: ev, Human: true, Addressed: "direct"}
		got := p.Gate(tr, Thread{}, nil)
		if !got.Admissible || got.Strength != Explicit || got.Basis.Mode != "auto" || got.Basis.Via != "addressed" {
			t.Errorf("%s: %+v", ev, got)
		}
		tr.Human = false
		if p.Gate(tr, Thread{}, nil).Admissible != (ev != "person.answer") {
			t.Errorf("内置跟进属性 %s", ev)
		}
	}
}

func TestAdmits(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		strength Strength
		via      string
		rules    []string
		want     string
	}{
		{"", Explicit, "event", nil, "refuse"}, {"off", Explicit, "followup", nil, "refuse"},
		{"observe", Explicit, "followup", nil, "ok"}, {"observe", Explicit, "addressed", nil, "observe"},
		{"auto", Explicit, "event", nil, "ok"}, {"observe", Inferred, "event", []string{"x"}, "observe"},
		{"auto", Inferred, "event", nil, "refuse"}, {"auto", Inferred, "event", []string{"on", "other"}, "ok"},
		{"auto", Inferred, "event", []string{"on", "off"}, "refuse"}, {"auto", Inferred, "event", []string{"missing"}, "refuse"},
	} {
		s := Settings{Rules: []Rule{{ID: "on", Enabled: true}, {ID: "other", Enabled: true}, {ID: "off"}}}
		var defs []EventDef
		if tc.mode != "" {
			defs = []EventDef{{Source: "gh", Event: "evt", Default: tc.mode}}
		}
		e := Evaluation{Strength: tc.strength, Basis: MandateBasis{Source: "gh", Event: "evt", Via: tc.via}}
		if got := buildPolicy(t, s, defs...).Admits(e, tc.rules); got != tc.want {
			t.Errorf("%+v 得到 %s", tc, got)
		}
	}
}

func TestRevalidate(t *testing.T) {
	actor := ActorRef{Source: "chat", ID: "u"}
	for _, tc := range []struct {
		mode        string
		strength    Strength
		via, sender string
		allowed     bool
		rules       []string
		want        bool
	}{
		{"", Explicit, "humanDirect", "", true, nil, false}, {"off", Explicit, "personRequest", "", true, nil, false},
		{"auto", Explicit, "followup", "u", false, nil, false}, {"observe", Explicit, "addressed", "u", false, nil, true},
		{"observe", Explicit, "event", "u", true, []string{"missing"}, true}, {"auto", Explicit, "event", "", false, nil, true},
		{"auto", Inferred, "event", "", false, nil, true}, {"observe", Inferred, "event", "", false, []string{"on"}, false},
		{"auto", Inferred, "event", "u", true, []string{"on"}, true}, {"auto", Inferred, "event", "", false, []string{"on", "off"}, false},
		{"auto", Inferred, "event", "", false, []string{"missing"}, false},
	} {
		s := Settings{Rules: []Rule{{ID: "on", Enabled: true}, {ID: "off"}}}
		if tc.allowed {
			s.AllowedSenders = []ActorRef{actor}
		}
		var defs []EventDef
		if tc.mode != "" {
			defs = []EventDef{{Source: "gh", Event: "evt", Default: tc.mode}}
		}
		m := Mandate{Actor: actor, Strength: tc.strength, Basis: MandateBasis{Source: "gh", Event: "evt", Via: tc.via, Sender: tc.sender, Rules: tc.rules}}
		if got := buildPolicy(t, s, defs...).Revalidate(m); got != tc.want {
			t.Errorf("%+v 得到 %t", tc, got)
		}
	}
}

func TestEffectivePolicyVersion(t *testing.T) {
	rule := EventRule{Name: "处理", Instruction: "<x>"}
	defs := []EventDef{{Source: "gh", Event: "z", Default: "auto", Rule: &rule}, {Source: "gh", Event: "a.b", Rule: &rule}}
	settings := Settings{AllowedSenders: []ActorRef{{Source: "gh", ID: "z"}, {Source: "chat", ID: "u", Name: "周"}}, Rules: []Rule{
		{ID: "user2", Name: "B", Enabled: true}, {ID: "gh__z", Name: "覆盖"}, {ID: "user1", Name: "A"},
	}}
	canonical := `{"allowedSenders":[{"id":"u","name":"周","source":"chat"},{"id":"z","source":"gh"}],"events":{"craft:person.answer":"auto","craft:person.direct":"auto","craft:person.request":"auto","gh:a.b":"observe","gh:z":"auto"},"rules":[{"enabled":true,"id":"gh__a_b","instruction":"<x>","name":"处理"},{"enabled":false,"id":"gh__z","instruction":"","name":"覆盖"},{"enabled":true,"id":"user2","instruction":"","name":"B"},{"enabled":false,"id":"user1","instruction":"","name":"A"}]}`
	p := buildPolicy(t, settings, defs...)
	if want := Key("policy-v1", canonical); p.Version() != want {
		t.Fatalf("版本 %s，期望 %s", p.Version(), want)
	}
	e := Evaluation{Strength: Inferred, Basis: MandateBasis{Source: "gh", Event: "z"}}
	if p.Admits(e, []string{"gh__z"}) != "refuse" || p.Admits(e, []string{"gh__a_b"}) != "ok" {
		t.Fatal("生效规则与准入不一致")
	}
	slices.Reverse(defs)
	slices.Reverse(settings.AllowedSenders)
	settings.Events = map[string]EventOverride{"gh:z": {Mode: "auto"}, "other:removed": {Mode: "auto"}}
	settings.Paused = true
	settings.EvalTimeoutMs = 99
	defs[0].OnUnmatched = "record"
	defs[0].FeedTier = "notice"
	defs[0].Verb = "处理"
	defs[0].NoTarget = "scratch"
	defs[0].Collapse = &Collapse{Group: "g", By: "x"}
	defs[0].Label = "改名"
	defs[0].Strength = Explicit
	defs[0].FollowupOnly = true
	if got := buildPolicy(t, settings, defs...).Version(); got != p.Version() {
		t.Errorf("非版本字段导致改版 %s", got)
	}
	for _, change := range []func(*Settings){
		func(s *Settings) { s.Events = map[string]EventOverride{"gh:z": {Mode: "observe"}} },
		func(s *Settings) { s.AllowedSenders = nil }, func(s *Settings) { s.Rules = s.Rules[:2] },
	} {
		s := settings
		change(&s)
		if buildPolicy(t, s, defs...).Version() == p.Version() {
			t.Error("生效设置变化没有改版")
		}
	}
	rule.Instruction = "更新"
	if buildPolicy(t, settings, defs...).Version() == p.Version() {
		t.Error("默认规则变化没有改版")
	}
}

func TestPolicyOwnsItsSnapshot(t *testing.T) {
	actor := ActorRef{Source: "chat", ID: "u"}
	s := Settings{AllowedSenders: []ActorRef{actor}, Events: map[string]EventOverride{"gh:evt": {Mode: "auto"}}, Rules: []Rule{{ID: "r", Enabled: true}}}
	defs := []EventDef{{Source: "gh", Event: "evt"}}
	p := buildPolicy(t, s, defs...)
	version := p.Version()
	s.AllowedSenders[0].ID = "v"
	s.Events["gh:evt"] = EventOverride{Mode: "off"}
	s.Rules[0].Enabled = false
	defs[0].Event = "changed"
	p.EffectiveRules()[0].Enabled = false
	for range 4 {
		t.Run("并发读取", func(t *testing.T) {
			t.Parallel()
			tr := Trigger{Source: "gh", Event: "evt", Actor: actor, Human: true}
			g := p.Gate(tr, Thread{Subscriptions: []string{"w"}}, nil)
			if g.Strength != Explicit || g.Basis.Sender != "" || p.ModeOf("gh", "evt") != "auto" || p.Version() != version || p.Admits(Evaluation{Strength: Inferred, Basis: g.Basis}, []string{"r"}) != "ok" {
				t.Error("构造后的策略受到外部修改")
			}
		})
	}
}

func TestRejectAmbiguousCatalogAndRules(t *testing.T) {
	entry := EventDef{Source: "gh", Event: "item"}
	for _, tc := range []struct {
		name string
		s    Settings
		defs []EventDef
	}{
		{"重复事件", Settings{}, []EventDef{entry, entry}},
		{"冒用内置声明", Settings{}, []EventDef{{Source: "craft", Event: "person.request"}}},
		{"缺少事件来源", Settings{}, []EventDef{{Event: "changed"}}},
		{"无效模式", Settings{Events: map[string]EventOverride{"gh:item": {Mode: "bad"}}}, []EventDef{entry}},
		{"重复用户规则", Settings{Rules: []Rule{{ID: "r"}, {ID: "r"}}}, nil},
		{"默认规则标识歧义", Settings{}, []EventDef{{Source: "gh", Event: "a.b", Rule: &EventRule{Name: "A", Instruction: "x"}}, {Source: "gh", Event: "a_b", Rule: &EventRule{Name: "B", Instruction: "y"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewPolicy(tc.s, tc.defs); err == nil {
				t.Fatal("意外接受无效策略")
			}
		})
	}
}

func TestSnapshotIsolatesEventDeclarations(t *testing.T) {
	def := EventDef{Source: "gh", Event: "item", Rule: &EventRule{Name: "检查", Instruction: "初始"}, Collapse: &Collapse{Group: "一组"}}
	p := buildPolicy(t, Settings{}, def)
	def.Rule.Instruction = "构造后改变"
	def.Collapse.Group = "其他组"
	result, _ := p.Event("gh", "item")
	result.Rule.Instruction = "读取后改变"
	result.Collapse.Group = "新的组"
	unchanged, _ := p.Event("gh", "item")
	if unchanged.Rule.Instruction != "初始" || unchanged.Collapse.Group != "一组" {
		t.Fatal("事件声明被外部改写")
	}
}
