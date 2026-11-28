package engine

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestWorkAttention(t *testing.T) {
	long := strings.Repeat("🙂", 601)
	for _, tc := range []struct {
		kind    string
		w       Work
		actions []string
	}{
		{"answer", Work{Attention: &Question{ID: "q", Question: long}}, []string{"answer", "stop"}},
		{"delivery", Work{Delivery: &DeliveryFailure{Error: long}}, []string{"retry", "mark-delivered", "stop"}},
		{"review", reported(Ready), []string{"stop"}}, {"blocked", reported(Blocked), []string{"retry", "stop"}}, {"failed", reported(Failed), []string{"retry", "stop"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			w := tc.w
			w.ID = "w"
			w.Title = long
			w.UpdatedAt = at1
			if len(w.Assignments) > 0 {
				w.Assignments[0].Report.Summary = long
			}
			set := AttentionSet{Works: []Work{w}}
			items := Attention(set)
			if len(items) != 1 {
				t.Fatal(items)
			}
			item := items[0]
			wantID := Key("attention", tc.kind, "w")
			if tc.kind == "answer" {
				wantID = Key("attention", "answer", "q", "w")
			}
			if item.Kind != tc.kind || item.Severity != "action" || item.Ref.Work != "w" || item.ID != wantID || !slices.Equal(item.Actions, tc.actions) || item.At != at1 {
				t.Fatal(item)
			}
			if utf8.RuneCountInString(item.Detail) != 600 || utf8.RuneCountInString(item.Title) != 600 {
				t.Fatal("文本截断错误")
			}
			switch tc.kind {
			case "answer":
				w.Attention.AnsweredAt = at2
			case "delivery":
				w.Delivery = nil
			case "review":
				w.Ended = &WorkEnd{}
			default:
				w.Assignments = append(w.Assignments, Assignment{State: "pending"})
			}
			set.Works[0] = w
			if got := Attention(set); len(got) != 0 {
				t.Fatalf("处理后仍出现 %+v", got)
			}
		})
	}
}

func TestUnavailableVisibility(t *testing.T) {
	failure := Decision{ID: "d", ThreadKey: "k", Kind: Unavailable, At: at1, TriggerIDs: []string{"x", "y"}, Reason: []Reason{{Code: "timeout"}}}
	for _, tc := range []struct {
		name                string
		pending, evaluating []string
		later               *Decision
		reasons             []Reason
		dismissed           bool
		trigger             string
	}{
		{"显示首个", nil, nil, nil, nil, false, "x"},
		{"部分待重评", []string{"x"}, nil, nil, nil, false, "y"},
		{"已在重评", []string{"x"}, []string{"y"}, nil, nil, false, ""},
		{"后续处理一个", nil, nil, &Decision{Kind: Ignore, TriggerIDs: []string{"x"}}, nil, false, "y"},
		{"后续处理全部", nil, nil, &Decision{Kind: Accept, TriggerIDs: []string{"x", "y"}}, nil, false, ""},
		{"已忽略", nil, nil, nil, nil, true, ""},
		{"内部重排", nil, nil, nil, []Reason{{Code: "policyChanged"}, {Code: "acceptedBefore"}}, false, ""},
		{"已路由的批内成员", nil, nil, nil, []Reason{{Code: "routed", TriggerID: "x"}, {Code: "timeout"}}, false, "y"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			th := Thread{Key: "k", Meta: ThreadMeta{Label: "对象"}, Pending: tc.pending}
			if tc.evaluating != nil {
				th.Evaluating = &Evaluation{TriggerIDs: tc.evaluating}
			}
			d := failure
			d.Dismissed = tc.dismissed
			if tc.reasons != nil {
				d.Reason = tc.reasons
			}
			set := AttentionSet{Threads: []Thread{th}, Decisions: []Decision{d}}
			if tc.later != nil {
				next := *tc.later
				next.At = at2
				next.ThreadKey = "k"
				next.ID = "next"
				set.Decisions = append([]Decision{next}, set.Decisions...)
			}
			items := Attention(set)
			if tc.trigger == "" {
				if len(items) != 0 {
					t.Fatal(items)
				}
				return
			}
			if len(items) != 1 {
				t.Fatal(items)
			}
			item := items[0]
			if item.Ref.Trigger != tc.trigger || item.Ref.Thread != "k" || item.Ref.Decision != "d" || item.Title != th.Meta.Label || item.Kind != "unavailable" || item.Severity != "warning" || !slices.Equal(item.Actions, []string{"reevaluate", "dismiss"}) {
				t.Fatal(item)
			}
		})
	}
}

func TestAttentionOrder(t *testing.T) {
	set := AttentionSet{Works: []Work{reported(Ready), reported(Failed), reported(Blocked)}, Threads: []Thread{{Key: "k"}}, Decisions: []Decision{
		{ID: "d1", ThreadKey: "k", At: at1, Kind: Unavailable, TriggerIDs: []string{"a"}},
		{ID: "d2", ThreadKey: "k", At: at2, Kind: Unavailable, TriggerIDs: []string{"b"}},
	}}
	for i := range set.Works {
		set.Works[i].ID = []string{"a", "b", "c"}[i]
	}
	set.Works[2].Assignments[0].Report.At = at2
	items := Attention(set)
	if len(items) != 4 || items[0].Kind != "blocked" || items[1].ID >= items[2].ID || items[3].Ref.Decision != "d2" {
		t.Fatal(items)
	}
	slices.Reverse(set.Works)
	slices.Reverse(set.Decisions)
	if !reflect.DeepEqual(items, Attention(set)) {
		t.Fatal("顺序取决于输入次序")
	}
}
