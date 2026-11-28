// Package outboundtest 用相同的扩展替身检验不同仓储的外发行为。
package outboundtest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/pkg/protocol"
)

type extension struct {
	preview            outbound.Reply
	calls              []protocol.ActionExecuteParams
	verify             []protocol.ActionVerifyParams
	status             outbound.Verification
	verifyErr, sendErr error
	verifyReceipt      json.RawMessage
	previewErr         error
}

func (e *extension) Execute(_ context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	e.calls = append(e.calls, p)
	if p.Preview {
		return e.preview, e.previewErr
	}
	return outbound.Reply{Value: json.RawMessage(`{"n":9007199254740993}`)}, e.sendErr
}

func (e *extension) Verify(_ context.Context, p protocol.ActionVerifyParams) (outbound.Verification, json.RawMessage, error) {
	e.verify = append(e.verify, p)
	if e.verifyReceipt != nil {
		return e.status, e.verifyReceipt, e.verifyErr
	}
	return e.status, json.RawMessage(`{"checked":true}`), e.verifyErr
}

func setup(t *testing.T, box outbound.Outbox, e *extension, now *time.Time) *outbound.Service {
	t.Helper()
	s, err := outbound.New(outbound.Config{Outbox: box, Executor: e, Clock: func() time.Time { return *now }, Rate: outbound.RateLimit{Max: 2, Window: time.Minute},
		Actions: []outbound.Action{{ID: "a", Level: outbound.Auto, Reversible: true}, {ID: "c", Level: outbound.Confirm}, {ID: "n", Level: outbound.ConfirmNamed}, {ID: "d", Level: outbound.Deny}, {ID: "r", ReadOnly: true}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func request(action, key string) outbound.Request {
	return outbound.Request{Action: action, IdempotencyKey: key, Params: json.RawMessage(`{"data":9007199254740993}`), Source: outbound.Session}
}

// Run 为每个用例创建独立的仓储，所有时间推进均由用例控制。
func Run(t *testing.T, factory func(*testing.T) outbound.Outbox) {
	t.Helper()
	for _, tc := range []struct {
		name, action    string
		yes             bool
		named           string
		source          outbound.Source
		want            error
		previews, sends int
	}{
		{"自动预览再发", "a", false, "", outbound.Session, nil, 1, 1},
		{"自动同意", "a", true, "", outbound.Session, nil, 1, 1},
		{"只读直通", "r", false, "", outbound.Worker, nil, 0, 1},
		{"待确认", "c", false, "", outbound.Session, outbound.NeedsConfirmation, 1, 0},
		{"当场确认", "c", true, "", outbound.Session, nil, 1, 1},
		{"等候点名", "n", false, "one", outbound.Session, outbound.NeedsConfirmation, 1, 0},
		{"正确点名", "n", true, "one", outbound.Session, nil, 1, 1},
		{"没有点名", "n", true, "", outbound.Session, outbound.ErrDenied, 1, 0},
		{"错误点名", "n", true, "two", outbound.Session, outbound.ErrDenied, 1, 0},
		{"合并点名", "n", true, "one,two", outbound.Session, outbound.ErrDenied, 1, 0},
		{"拒绝", "d", false, "", outbound.Session, outbound.ErrDenied, 0, 0},
		{"同意也拒绝", "d", true, "", outbound.Session, outbound.ErrDenied, 0, 0},
		{"未登记", "new", false, "", outbound.Session, outbound.NeedsConfirmation, 1, 0},
		{"未登记确认", "new", true, "", outbound.Session, nil, 1, 1},
		{"后台同意", "c", true, "", outbound.Worker, outbound.NeedsConfirmation, 1, 0},
		{"后台点名", "n", true, "one", outbound.Worker, outbound.NeedsConfirmation, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &extension{preview: outbound.Reply{Summary: "summary", Target: "one"}}
			s := setup(t, factory(t), e, new(time.Unix(10, 0)))
			q := request(tc.action, "k")
			q.Yes, q.NamedTarget, q.Source = tc.yes, tc.named, tc.source
			r, err := s.Act(t.Context(), q)
			if !errors.Is(err, tc.want) {
				t.Fatalf("结果 %+v, %v", r, err)
			}
			if errors.Is(err, outbound.NeedsConfirmation) && (r.Preview.Summary != e.preview.Summary || r.State != outbound.Pending) {
				t.Errorf("待确认缺预览 %+v", r)
			}
			if tc.action == "a" && !r.Reversible {
				t.Error("预览缺少声明的可撤销性")
			}
			if len(e.calls) != tc.previews+tc.sends {
				t.Fatalf("扩展调用 %+v", e.calls)
			}
			for i, p := range e.calls {
				if p.Preview != (i < tc.previews) || p.Action != q.Action || p.IdempotencyKey != q.IdempotencyKey || string(p.Params) != string(q.Params) {
					t.Errorf("第 %d 次调用 %+v", i, p)
				}
			}
			if tc.sends > 0 && (r.State != outbound.Done || string(r.Receipt) != `{"n":9007199254740993}`) {
				t.Errorf("回执 %+v", r)
			}
		})
	}
	for _, tc := range []struct {
		name string
		run  func(*testing.T, outbound.Outbox)
	}{
		{"清单与重查", pending}, {"恢复", recovery}, {"窗口", rateWindow}, {"非法转移", transitions}, {"重复提交", duplicates}, {"并发提交", concurrent}, {"内容降级", content}, {"过期确认", staleConfirmation}, {"乱序准入", queuedWindow}, {"核实回执损坏", invalidEvidence}, {"重查拒绝", rejectOnRecheck}, {"未生效释放名额", releaseUnusedSlot},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.run(t, factory(t)) })
	}
}

func pending(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	now := time.Unix(10, 0)
	e := &extension{preview: outbound.Reply{Summary: "first", Target: "one"}}
	s := setup(t, box, e, &now)
	for _, key := range []string{"x", "y"} {
		q := request("n", key)
		q.Source = outbound.Worker
		if _, err := s.Act(ctx, q); !errors.Is(err, outbound.NeedsConfirmation) {
			t.Fatal(err)
		}
	}
	items, err := s.PendingConfirmations(ctx)
	if err != nil || len(items) != 2 {
		t.Fatalf("清单 %+v, %v", items, err)
	}
	item := items[0]
	if item.ID != "x" || item.Action != "n" || item.Preview != "first" || item.Source != outbound.Worker || !item.At.Equal(now) {
		t.Errorf("清单项 %+v", item)
	}
	e.preview.Target = "two"
	if _, err = s.Confirm(ctx, "x", "one"); !errors.Is(err, outbound.ErrDenied) {
		t.Fatalf("目标已变 %v", err)
	}
	if len(e.calls) != 3 || !e.calls[2].Preview {
		t.Fatal(e.calls)
	}
	if err = s.Withdraw(ctx, "y"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Confirm(ctx, "y", "two"); err == nil {
		t.Fatal("已撤回仍可确认")
	}
	if _, err = s.Confirm(ctx, "x", "two"); err != nil {
		t.Fatal(err)
	}
	items, err = s.PendingConfirmations(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("处理后的清单 %+v, %v", items, err)
	}
	all, err := s.List(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("外发记录 %+v, %v", all, err)
	}
	r, err := s.Show(ctx, "x")
	if err != nil || r.Source != outbound.Worker || r.State != outbound.Done || r.SentSeq <= 0 || len(r.Receipt) == 0 {
		t.Fatalf("持久回执 %+v, %v", r, err)
	}
}

func recovery(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	for _, tc := range []struct {
		key       string
		status    outbound.Verification
		verifyErr error
		want      outbound.State
	}{
		{"yes", outbound.Applied, nil, outbound.Done}, {"no", outbound.NotApplied, nil, outbound.Failed}, {"maybe", outbound.Inconclusive, nil, outbound.Unknown}, {"error", "", errors.New("offline"), outbound.Unknown},
	} {
		e := &extension{sendErr: errors.New("disconnected")}
		s := setup(t, box, e, new(time.Unix(10, 0)))
		q := request("c", tc.key)
		q.Yes = true
		if _, err := s.Act(ctx, q); err == nil {
			t.Fatal("发送中断应上报")
		}
		r, err := s.Show(ctx, q.IdempotencyKey)
		if err != nil || r.State != outbound.Executing {
			t.Fatalf("中断记录 %+v, %v", r, err)
		}
		restarted := &extension{status: tc.status, verifyErr: tc.verifyErr}
		s = setup(t, box, restarted, new(time.Unix(20, 0)))
		err = s.Recover(ctx)
		if !errors.Is(err, tc.verifyErr) {
			t.Fatal(err)
		}
		if err = s.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		r, err = s.Show(ctx, q.IdempotencyKey)
		if err != nil || r.State != tc.want || (r.State == outbound.Done && len(r.Receipt) == 0) {
			t.Fatalf("核实 %+v, %v", r, err)
		}
		_, err = s.Act(ctx, q)
		if (err == nil) != (tc.want == outbound.Done) {
			t.Fatalf("恢复后重放 %s: %v", tc.want, err)
		}
		if len(restarted.calls) != 0 || len(restarted.verify) != 1 || restarted.verify[0] != (protocol.ActionVerifyParams{Action: q.Action, IdempotencyKey: q.IdempotencyKey}) {
			t.Fatalf("恢复调用 %+v", restarted)
		}
	}
}

func rateWindow(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	now := time.Unix(0, 0)
	e := &extension{}
	for i, tc := range []struct {
		key   string
		after time.Duration
		want  error
	}{
		{"a", 0, nil}, {"a", 0, nil}, {"b", time.Second, nil}, {"c", 58 * time.Second, outbound.NeedsConfirmation}, {"d", time.Second, nil},
	} {
		now = now.Add(tc.after)
		// 各次提交模拟不同的短进程，共用数据库中的滑动窗口。
		s := setup(t, box, e, &now)
		r, err := s.Act(ctx, request("a", tc.key))
		if !errors.Is(err, tc.want) {
			t.Fatalf("%d: %+v %v", i, r, err)
		}
	}
	now = now.Add(time.Hour)
	q := request("a", "c")
	if _, err := setup(t, box, e, &now).Act(ctx, q); !errors.Is(err, outbound.NeedsConfirmation) {
		t.Fatal("已转入确认的请求须保留", err)
	}
}

func transitions(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	clock := func() time.Time { return time.Unix(0, 0) }
	r := outbound.Result{ID: "k", Revision: 1, Action: "a", State: outbound.Done}
	if err := box.Save(ctx, &r, outbound.RateLimit{}, clock); err == nil {
		t.Fatal("跳过执行直接完成")
	}
	r.State = outbound.Previewing
	if err := box.Save(ctx, &r, outbound.RateLimit{}, clock); err != nil {
		t.Fatal(err)
	}
	r.Revision++
	r.State = outbound.Pending
	if err := box.Save(ctx, &r, outbound.RateLimit{}, clock); err != nil {
		t.Fatal(err)
	}
	r.State = outbound.Withdrawn
	if err := box.Save(ctx, &r, outbound.RateLimit{}, clock); err == nil {
		t.Fatal("接受过期的前态")
	}
	r.Revision++
	if err := box.Save(ctx, &r, outbound.RateLimit{}, clock); err != nil {
		t.Fatal(err)
	}
	r.Revision++
	r.State = outbound.Executing
	if err := box.Save(ctx, &r, outbound.RateLimit{}, clock); err == nil {
		t.Fatal("执行已撤回项")
	}
}

func duplicates(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	e := &extension{}
	s := setup(t, box, e, new(time.Unix(10, 0)))
	q := request("a", "same")
	for range 2 {
		if _, err := s.Act(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	q.Params = json.RawMessage(`{"data":9007199254740992}`)
	if _, err := s.Act(ctx, q); err == nil {
		t.Fatal("同键替换了请求")
	}
	if len(e.calls) != 2 {
		t.Fatalf("重复执行 %+v", e.calls)
	}
}

func content(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	for _, tc := range []struct {
		key      string
		params   json.RawMessage
		preview  string
		injected bool
	}{
		{"credential", json.RawMessage(`{"\u0074oken":"value"}`), "", false},
		{"encoded", json.RawMessage(`{"body":["\u002fhome/person/file"]}`), "", false},
		{"preview", json.RawMessage(`{}`), "/home/person/file", false},
		{"custom", json.RawMessage(`{"body":"custom"}`), "", true},
	} {
		e := &extension{preview: outbound.Reply{Summary: tc.preview}}
		cfg := outbound.Config{Outbox: box, Executor: e, Clock: func() time.Time { return time.Unix(10, 0) }, Rate: outbound.RateLimit{Max: 2, Window: time.Minute}, Actions: []outbound.Action{{ID: "a", Level: outbound.Auto}}}
		if tc.injected {
			cfg.Content = outbound.ContentRules{func(s string) bool { return s == "custom" }}
		}
		s, err := outbound.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		q := request("a", tc.key)
		q.Params = tc.params
		r, err := s.Act(ctx, q)
		if !errors.Is(err, outbound.NeedsConfirmation) || r.Level != outbound.Confirm || len(e.calls) != 1 || !e.calls[0].Preview {
			t.Fatalf("检查 %+v %v %+v", r, err, e.calls)
		}
		if _, err = s.Confirm(ctx, tc.key, ""); err != nil {
			t.Fatal(err)
		}
	}
}

func invalidEvidence(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	e := &extension{sendErr: errors.New("disconnected"), status: outbound.Applied, verifyReceipt: json.RawMessage("{")}
	s := setup(t, box, e, new(time.Unix(10, 0)))
	q := request("c", "broken")
	q.Yes = true
	if _, err := s.Act(ctx, q); err == nil {
		t.Fatal("发送中断应上报")
	}
	if err := s.Recover(ctx); err == nil {
		t.Fatal("损坏的核实结果应上报")
	}
	r, err := s.Show(ctx, q.IdempotencyKey)
	if err != nil || r.State != outbound.Unknown || len(e.verify) != 1 {
		t.Fatalf("核实缺少有效回执 %+v %v", r, err)
	}
}

func rejectOnRecheck(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	e := &extension{preview: outbound.Reply{Target: "one"}}
	s := setup(t, box, e, new(time.Unix(10, 0)))
	q := request("c", "changed")
	if _, err := s.Act(ctx, q); !errors.Is(err, outbound.NeedsConfirmation) {
		t.Fatal(err)
	}
	q.Yes = true
	e.previewErr = outbound.ErrDenied
	r, err := s.Act(ctx, q)
	if !errors.Is(err, outbound.ErrDenied) || r.State != outbound.Failed || len(e.calls) != 2 || !e.calls[1].Preview {
		t.Fatalf("硬规则已变 %+v, %v, %+v", r, err, e.calls)
	}
}

func releaseUnusedSlot(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	e := &extension{sendErr: errors.New("disconnected"), status: outbound.NotApplied}
	s := setup(t, box, e, new(time.Unix(10, 0)))
	if _, err := s.Act(ctx, request("a", "interrupted")); err == nil {
		t.Fatal("发送中断应上报")
	}
	if err := s.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	e.sendErr = nil
	for _, key := range []string{"one", "two"} {
		if _, err := s.Act(ctx, request("a", key)); err != nil {
			t.Fatalf("明确未生效的发送仍占用名额: %v", err)
		}
	}
}
