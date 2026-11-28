package engine

import (
	"strconv"
	"strings"
	"testing"
)

var benchmarkSettings = Settings{
	AllowedSenders: []ActorRef{{Source: "chat", ID: "u"}},
	Rules:          []Rule{{ID: "r", Enabled: true}},
}
var benchmarkCatalog = []EventDef{{Source: "gh", Event: "update", Default: "auto", FollowupOnly: true}}

func hotPolicy(t testing.TB) Policy {
	return buildPolicy(t, benchmarkSettings, benchmarkCatalog...)
}

var (
	hotRef     = NewThreadRef("gh", "item", strings.Repeat("long-id-", 12))
	hotTrigger = Trigger{Source: "gh", Event: "update", Actor: ActorRef{Source: "chat", ID: "u"}, Human: true}
	hotThread  = Thread{Context: []string{"reader"}}
	hotWorks   = []Work{{ID: "reader"}, {ID: "owner"}}
	hotEval    = Evaluation{Strength: Inferred, Basis: MandateBasis{Source: "gh", Event: "update", Via: "event", Rules: []string{"r"}}}
	hotWork    = reported(Blocked)
	sinkGate   GateResult
	sinkStage  Stage
	sinkString string
	sinkValid  bool
)

func TestHotAllocations(t *testing.T) {
	p := hotPolicy(t)
	for _, tc := range []struct {
		name string
		run  func()
	}{
		{"Gate", func() { sinkGate = p.Gate(hotTrigger, hotThread, hotWorks) }},
		{"Admits", func() { sinkString = p.Admits(hotEval, hotEval.Basis.Rules) }},
		{"Revalidate", func() { sinkValid = p.Revalidate(Mandate{Strength: Inferred, Basis: hotEval.Basis}) }},
		{"ModeOf", func() { sinkString = p.ModeOf("gh", "update") }},
		{"WorkStage", func() { sinkStage = WorkStage(hotWork) }}, {"ThreadKey", func() { sinkString = ThreadKey(hotRef) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := testing.AllocsPerRun(1000, tc.run); got != 0 {
				t.Fatalf("%v allocs/op", got)
			}
		})
	}
}

func BenchmarkGate(b *testing.B) {
	p := hotPolicy(b)
	b.ReportAllocs()
	for b.Loop() {
		sinkGate = p.Gate(hotTrigger, hotThread, hotWorks)
	}
}

func BenchmarkAdmits(b *testing.B) {
	p := hotPolicy(b)
	b.ReportAllocs()
	for b.Loop() {
		sinkString = p.Admits(hotEval, hotEval.Basis.Rules)
	}
}

func BenchmarkRevalidate(b *testing.B) {
	p := hotPolicy(b)
	m := Mandate{Strength: Inferred, Basis: hotEval.Basis}
	b.ReportAllocs()
	for b.Loop() {
		sinkValid = p.Revalidate(m)
	}
}

func BenchmarkModeOf(b *testing.B) {
	p := hotPolicy(b)
	b.ReportAllocs()
	for b.Loop() {
		sinkString = p.ModeOf("gh", "update")
	}
}

func BenchmarkWorkStage(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkStage = WorkStage(hotWork)
	}
}

func BenchmarkThreadKey(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		sinkString = ThreadKey(hotRef)
	}
}

func BenchmarkPolicyVersion(b *testing.B) {
	p := hotPolicy(b)
	b.ReportAllocs()
	for b.Loop() {
		sinkString = p.Version()
	}
}

func BenchmarkNewPolicy(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewPolicy(benchmarkSettings, benchmarkCatalog); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAttention(b *testing.B) {
	for _, n := range []int{100, 1000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			var set AttentionSet
			for i := range n {
				id := strconv.Itoa(i)
				w := reported(Blocked)
				w.ID = id
				set.Works = append(set.Works, w)
				set.Threads = append(set.Threads, Thread{Key: id})
				set.Decisions = append(set.Decisions, Decision{ID: id, Kind: Unavailable, ThreadKey: id, TriggerIDs: []string{id}, At: at1})
			}
			b.ReportAllocs()
			for b.Loop() {
				_ = Attention(set)
			}
		})
	}
}
