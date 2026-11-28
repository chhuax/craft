package engine

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

const at1 = "2025-05-06T10:00:00.001Z"
const at2 = "2025-05-06T10:00:00.002Z"

func sameJSON(t *testing.T, wire string, value any) {
	t.Helper()
	if err := json.Unmarshal([]byte(wire), value); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	for i, doc := range []string{wire, string(out)} {
		dst := &a
		if i == 1 {
			dst = &b
		}
		dec := json.NewDecoder(strings.NewReader(doc))
		dec.UseNumber()
		if err := dec.Decode(dst); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(a, b) {
		t.Errorf("往返后得到 %s，期望 %s", out, wire)
	}
}

func TestModelDocuments(t *testing.T) {
	for _, tc := range []struct {
		name, wire string
		value      any
	}{
		{"trigger", `{
  "id": "e",
  "source": "gh",
  "event": "item",
  "threads": [{"source": "gh", "kind": "item", "id": "42"}],
  "actor": {"source": "chat", "id": "u"},
  "human": true,
  "addressed": "mention",
  "at": "2025-05-06T10:00:00.002Z",
  "text": "消息",
  "channel": {"source": "chat", "thread": {"room": "r"}, "other": [9007199254740993]},
  "meta": {"n": 9007199254740993}
}`, &Trigger{}},
		{"disposition", `{"filtered":{"code":"modeOff"},"recorded":true,"evalId":"e","decisionId":"d","fanout":[],"context":["w"],"requeued":true,"handbacks":[{"workId":"w","reason":"x","at":"2025-05-06T10:00:00.002Z"}],"at":"2025-05-06T10:00:00.002Z"}`, &TriggerDisposition{}},
		{"thread", `{
  "key": "gh:item:1",
  "ref": {"source": "gh", "kind": "item", "id": "1"},
  "createdAt": "2025-05-06T10:00:00.002Z",
  "lastAt": "2025-05-06T10:00:00.003Z",
  "subscriptions": ["w"],
  "context": ["c"],
  "correlated": [],
  "pending": ["p"],
  "evaluating": {"id": "e", "threadKey": "gh:item:1", "triggerIds": ["p"], "primary": "p", "strength": "explicit", "actor": {"source": "gh", "id": "u"}, "basis": {"via": "event", "source": "gh", "event": "item"}, "policyVersion": "v", "startedAt": "2025-05-06T10:00:00.002Z"},
  "closed": {"at": "2025-05-06T10:00:00.002Z", "outcome": "done", "detail": "ok"},
  "reopenedAt": "2025-05-06T10:00:00.003Z",
  "meta": {"label": "A", "url": "u", "facts": {"title": {"value": "标题"}, "custom": {"value": [3, true], "label": "值", "problem": true}}}
}`, &Thread{}},
		{"decision", `{
  "id": "d",
  "evalId": "e",
  "threadKey": "k",
  "triggerIds": [],
  "policyVersion": "v",
  "at": "2025-05-06T10:00:00.002Z",
  "kind": "accept",
  "reason": [{"code": "said", "values": {"text": "理由"}, "triggerId": "p"}],
  "strength": "explicit",
  "workId": "w",
  "mandate": {"strength": "explicit", "policyVersion": "v", "basis": {"via": "addressed", "source": "chat", "event": "m", "sender": "人", "mode": "observe", "rules": ["r"]}, "actor": {"source": "chat", "id": "u"}, "scope": "一个对象"},
  "audit": {"evidence": ["a"]},
  "dismissed": true
}`, &Decision{}},
		{"work", workDocument, &Work{}},
		{"effect", `{"kind":"launch","key":"a","value":["w",9007199254740993],"seq":3,"generation":2,"attempts":1,"error":"x","due":"2025-05-06T10:00:00.002Z","since":"2025-05-06T10:00:00.001Z"}`, &Effect{}},
	} {
		t.Run(tc.name, func(t *testing.T) { sameJSON(t, tc.wire, tc.value) })
	}
}

const workDocument = `{
  "id": "w",
  "title": "测试",
  "decisionId": "d",
  "mandate": {"strength": "explicit", "policyVersion": "v", "basis": {"via": "event", "source": "gh", "event": "assigned"}, "actor": {"source": "gh", "id": "u"}, "scope": "a"},
  "channels": [{"source": "chat", "thread": "r", "sub": 123}],
  "ended": {"at": "2025-05-06T10:00:00.002Z", "by": "executor", "outcome": "done", "reason": "ok"},
  "attention": {"id": "q", "question": "?", "triggerId": "e", "createdAt": "2025-05-06T10:00:00.001Z", "answeredAt": "2025-05-06T10:00:00.002Z"},
  "assignments": [{"id": "a", "state": "ended", "createdAt": "2025-05-06T10:00:00.001Z", "startedAt": "2025-05-06T10:00:00.001Z", "endedAt": "2025-05-06T10:00:00.002Z", "endedReason": "done", "report": {"state": "ready", "summary": "ok", "at": "2025-05-06T10:00:00.002Z"}, "deliveredAt": "2025-05-06T10:00:00.001Z", "idleAt": "2025-05-06T10:00:00.002Z"}],
  "inputs": [{"kind": "trigger", "id": "i", "at": "2025-05-06T10:00:00.001Z", "triggerId": "e", "human": true, "note": "n", "context": true, "deliveredTo": "a", "deliveredAt": "2025-05-06T10:00:00.002Z", "readAt": null}, {"kind": "text", "id": "j", "at": "2025-05-06T10:00:00.002Z", "text": ""}],
  "undelivered": [],
  "follows": [{"thread": "gh:item:1", "own": true, "via": ["e"], "manual": {"own": false}, "closed": {"at": "2025-05-06T10:00:00.002Z", "outcome": "closed"}}],
  "origin": {"triggerId": "e", "triggerIds": ["e"]},
  "execution": {"target": {"id": "r", "kind": "dir", "path": "dir", "title": "目录", "evidence": ["x"], "selectedBy": "rule"}, "directory": "dir", "session": {"host": "local", "id": "s"}, "bootstrapped": true, "assignment": "a"},
  "delivery": {"error": "x", "at": "2025-05-06T10:00:00.002Z"},
  "createdAt": "2025-05-06T10:00:00.001Z",
  "updatedAt": "2025-05-06T10:00:00.002Z"
}`

func TestWorkInputReadAt(t *testing.T) {
	var in WorkInput
	for _, tc := range []struct {
		field   string
		present bool
		time    string
	}{
		{`,"readAt":"` + at1 + `"`, true, at1}, {`,"readAt":null`, true, ""}, {"", false, ""},
	} {
		sameJSON(t, `{"kind":"trigger","id":"i","at":"2025-05-06T10:00:00.002Z","triggerId":"e"`+tc.field+`}`, &in)
		if in.ReadAt.Present != tc.present || in.ReadAt.Time != tc.time {
			t.Errorf("readAt = %+v", in.ReadAt)
		}
	}
}

func TestWorkInputKinds(t *testing.T) {
	for _, fields := range []string{
		`"kind":"policy-changed","from":"a","to":"b","effect":{"rules":[{"id":"r","name":"n","change":"changed","instruction":"i"}],"proactive":false,"requesterAllowed":false,"requester":{"source":"gh","id":"u"}}`,
		`"kind":"reopen","decisionId":"d"`, `"kind":"resumed","triggerId":"e"`,
		`"kind":"text","text":""`, `"kind":"note","text":"备注"`,
	} {
		sameJSON(t, `{"id":"i","at":"2025-05-06T10:00:00.002Z",`+fields+`}`, &WorkInput{})
	}
	for _, wire := range []string{`{"kind":"other"}`, `{"kind":"trigger","readAt":false}`, `{"kind":"resumed","triggerId":"x","readAt":null}`} {
		var in WorkInput
		if err := json.Unmarshal([]byte(wire), &in); err == nil {
			t.Errorf("意外接受 %s", wire)
		}
	}
}

func TestNonFiniteRejectedAtMarshal(t *testing.T) {
	for _, bad := range []string{"NaN", "Infinity", "-Infinity"} {
		for _, v := range []any{Fact{Value: json.RawMessage(bad)}, Effect{Value: json.RawMessage(bad)}, Trigger{Meta: map[string]json.RawMessage{"n": json.RawMessage(bad)}}} {
			if _, err := json.Marshal(v); err == nil {
				t.Errorf("%T 接受了 %s", v, bad)
			}
		}
	}
}

func TestThreadKey(t *testing.T) {
	for _, id := range []string{"1", "part:2", strings.Repeat("长", 80)} {
		ref := NewThreadRef("gh", "item", id)
		wire, err := jsonv2.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		var restored ThreadRef
		if err := jsonv2.Unmarshal(wire, &restored); err != nil {
			t.Fatal(err)
		}
		for _, r := range []ThreadRef{ref, restored} {
			if got := ThreadKey(r); got != "gh:item:"+id {
				t.Fatalf("对象键不一致 %q", got)
			}
		}
	}
}

func BenchmarkWorkJSON(b *testing.B) {
	var w Work
	if err := json.Unmarshal([]byte(workDocument), &w); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		raw, err := json.Marshal(&w)
		if err != nil {
			b.Fatal(err)
		}
		var dst Work
		if err := json.Unmarshal(raw, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func TestChangedRuleCanClearInstruction(t *testing.T) {
	sameJSON(t, `{"id":"r","name":"检查","change":"changed","instruction":""}`, &RuleChange{})
}

func TestNullOptionalsDisappear(t *testing.T) {
	var w Work
	if err := json.Unmarshal([]byte(`{"ended":null,"attention":null,"execution":null,"delivery":null}`), &w); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ended", "attention", "execution", "delivery"} {
		if _, ok := fields[name]; ok {
			t.Errorf("空可选字段仍被写出 %s", name)
		}
	}
}

func TestInvalidInputsFailBeforePersistence(t *testing.T) {
	for _, in := range []WorkInput{
		{Kind: "other"},
		{Kind: "resumed"},
		{Kind: "policy-changed", From: "old"},
		{Kind: "reopen"},
		{Kind: "note"},
		{Kind: "trigger", TriggerID: "e", ReadAt: ReadTime{Present: true, Time: "invalid"}},
	} {
		if _, err := json.Marshal(Work{Inputs: []WorkInput{in}}); err == nil {
			t.Errorf("接受无效输入 %+v", in)
		}
	}
}

func TestEmptyCollectionsInNewAggregates(t *testing.T) {
	for _, tc := range []struct {
		aggregate      any
		lists, objects []string
	}{
		{Work{}, []string{"channels", "assignments", "inputs", "undelivered"}, nil},
		{Thread{}, []string{"subscriptions", "correlated", "pending"}, nil},
		{TriggerDisposition{}, []string{"fanout"}, nil},
		{Trigger{}, []string{"threads"}, []string{"meta"}},
		{Evaluation{}, []string{"triggerIds"}, nil},
		{Decision{}, []string{"triggerIds", "reason"}, nil},
	} {
		raw, err := json.Marshal(tc.aggregate)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for _, key := range tc.lists {
			if string(fields[key]) != "[]" {
				t.Errorf("%T.%s = %s", tc.aggregate, key, fields[key])
			}
		}
		for _, key := range tc.objects {
			if string(fields[key]) != "{}" {
				t.Errorf("%T.%s = %s", tc.aggregate, key, fields[key])
			}
		}
	}
}
