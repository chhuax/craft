package enginetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func threadMeta(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	ref := engine.NewThreadRef("feed", "item", "a")
	key := engine.ThreadKey(ref)
	label, url := "示例", "https://example.invalid/a"
	title := engine.Fact{Value: json.RawMessage(`"旧名"`)}
	problem := json.RawMessage(`{"code":"late"}`)
	custom := engine.Fact{Value: json.RawMessage(`{"count":9007199254740993}`), Label: "计数", Problem: &problem}
	var events []engine.Event
	s.Subscribe(func(ev engine.Event) { events = append(events, ev) })
	// patch 只有对象键，不能凭补丁构造新的来源引用。
	must(t, e.PatchThreadMeta(ctx, key, map[string]json.RawMessage{"label": json.RawMessage(`"尚未描述"`)}))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		th, err := r.GetThread(key)
		must(t, err)
		if th != nil || len(events) != 0 {
			t.Fatalf("未知对象的补丁产生状态或广播 %+v %v", th, events)
		}
		return nil
	}))
	must(t, e.DescribeThread(ctx, ref, &label, nil, map[string]*engine.Fact{"title": &title, "custom": &custom}))
	must(t, e.DescribeThread(ctx, ref, nil, &url, map[string]*engine.Fact{"title": nil, "due": {Value: json.RawMessage(`"2026-05-01"`)}}))
	must(t, e.PatchThreadMeta(ctx, key, map[string]json.RawMessage{"label": json.RawMessage(`"更新"`)}))
	must(t, e.PatchThreadMeta(ctx, key, map[string]json.RawMessage{"url": json.RawMessage(`null`)}))
	for _, raw := range []string{"null", "{}"} {
		err := e.PatchThreadMeta(ctx, key, map[string]json.RawMessage{"facts": json.RawMessage(raw), "label": json.RawMessage(`"意外改写"`)})
		var refusal *engine.UserError
		if !errors.As(err, &refusal) || !engine.IsRefused(err) {
			t.Fatalf("补丁改 facts 应被拒绝，得到 %v", err)
		}
	}
	must(t, s.Read(ctx, func(r engine.Reader) error {
		th, err := r.GetThread(key)
		must(t, err)
		want := engine.ThreadMeta{Label: "更新", Facts: map[string]engine.Fact{"custom": custom, "due": {Value: json.RawMessage(`"2026-05-01"`)}}}
		if th == nil || th.Ref != ref || th.CreatedAt != now || !reflect.DeepEqual(th.Meta, want) {
			t.Fatalf("合并、删除或拒绝补丁后的对象 %+v，期望元信息 %+v", th, want)
		}
		log, err := r.EventsSince(0, 8)
		must(t, err)
		if len(log) != 4 || !reflect.DeepEqual(events, log) {
			t.Fatalf("变更事件 %v，广播 %v", log, events)
		}
		for _, ev := range log {
			if ev.Ref["thread"] != key || ev.At != now {
				t.Errorf("对象通知缺少定位信息 %+v", ev)
			}
		}
		return nil
	}))
}
