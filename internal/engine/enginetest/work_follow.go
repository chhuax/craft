package enginetest

import (
	"slices"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func readWork(t *testing.T, s engine.Store, id string) engine.Work {
	t.Helper()
	var w *engine.Work
	must(t, s.Read(t.Context(), func(r engine.Reader) error { var err error; w, err = r.GetWork(id); return err }))
	if w == nil {
		t.Fatalf("缺少工作 %s", id)
	}
	return *w
}

func workSubscriptions(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("object")
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutWork(engine.Work{ID: "owner"}))
		return u.PutWork(engine.Work{ID: "reader"})
	}))
	for i := range 2 {
		added, err := e.FollowForContext(ctx, "reader", ref, "观察变化")
		must(t, err)
		if added != (i == 0) {
			t.Fatalf("第 %d 次只读跟随结果 %v", i, added)
		}
	}
	th := getThread(t, s, ref)
	if !slices.Equal(th.Context, []string{"reader"}) || !slices.Equal(th.Subscriptions, th.Context) {
		t.Fatalf("只读订阅 %+v", th)
	}
	tr := trigger("message", "signal", ref)
	tr.Human = true
	receive(t, e, tr, engine.Stored)
	if !readWork(t, s, "reader").Inputs[0].Context {
		t.Fatal("只读消息成为普通输入")
	}
	must(t, e.MarkUnrelated(ctx, "reader", ref, "其他工作"))
	checkCode(t, e.MarkUnrelated(ctx, "reader", ref, "重复"), engine.CodeNotFollowing)
	_, err := e.BindThread(ctx, "owner", ref, true)
	must(t, err)
	_, err = e.BindThread(ctx, "reader", ref, true)
	checkCode(t, err, engine.CodeThreadAlreadyOwned)
	_, err = e.BindThread(ctx, "reader", ref, false)
	must(t, err)
	checkCode(t, e.MarkUnrelated(ctx, "reader", ref, "手工绑定"), engine.CodeFollowedThread)
	for i := range 2 {
		ok, err := e.Unsubscribe(ctx, engine.ThreadKey(ref))
		must(t, err)
		if ok != (i == 0) {
			t.Fatal("重复退订结果错误")
		}
	}
	th = getThread(t, s, ref)
	if len(th.Context)+len(th.Subscriptions)+len(readWork(t, s, "owner").Follows)+len(readWork(t, s, "reader").Follows) != 0 {
		t.Fatal("退订后遗留关系")
	}
}

func returnedFollows(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	origin := item("origin")
	receive(t, e, trigger("start", "assign", origin), engine.Stored)
	d, err := e.Decide(ctx, engine.Outcome{Kind: engine.Accept, TriggerID: "start"})
	must(t, err)
	checkCode(t, e.MarkUnrelated(ctx, d.WorkID, origin, "无关"), engine.CodeSourceThread)
	for _, tc := range []struct {
		id          string
		manual, own bool
	}{{"auto", false, false}, {"manual", true, false}, {"owned", true, true}} {
		ref := item(tc.id)
		for _, suffix := range []string{"-a", "-b"} {
			tr := trigger(tc.id+suffix, "signal", ref)
			tr.Human = true
			receive(t, e, tr, engine.Stored)
		}
		checkDue(t, e, 1)
		_, err = e.Attach(ctx, d.WorkID, tc.id+"-a", "")
		must(t, err)
		checkDue(t, e, 1)
		_, err = e.Attach(ctx, d.WorkID, tc.id+"-b", "")
		must(t, err)
		if tc.manual {
			_, err = e.BindThread(ctx, d.WorkID, ref, tc.own)
			must(t, err)
		}
		for i, suffix := range []string{"-a", "-b"} {
			id := tc.id + suffix
			ok, err := e.Handback(ctx, d.WorkID, id, strings.Repeat("退", 300))
			must(t, err)
			if !ok || !slices.Contains(getThread(t, s, ref).Pending, id) {
				t.Fatal("未回到研判")
			}
			fs := readWork(t, s, d.WorkID).Follows
			idx := slices.IndexFunc(fs, func(f engine.Follow) bool { return f.Thread == engine.ThreadKey(ref) })
			switch {
			case i == 0:
				if idx < 0 || !fs[idx].Own || !slices.Equal(fs[idx].Via, []string{tc.id + "-b"}) {
					t.Fatalf("其他自动来源丢失 %+v", fs)
				}
			case tc.manual:
				if idx < 0 || fs[idx].Own != tc.own || len(fs[idx].Via) != 0 || fs[idx].Manual == nil || fs[idx].Manual.Own != tc.own {
					t.Fatalf("未恢复手工绑定 %+v", fs)
				}
			case idx >= 0 || slices.Contains(getThread(t, s, ref).Subscriptions, d.WorkID):
				t.Fatal("退回所有自动来源后仍占用对象")
			}
			_, err = e.Handback(ctx, d.WorkID, id, "再次退回")
			checkCode(t, err, engine.CodeHandedBack)
			_, err = e.Attach(ctx, d.WorkID, id, "再挂接")
			checkCode(t, err, engine.CodeHandedBackHere)
		}
		// 消费这一组退回的批次，各组只操作自己的研判。
		checkDue(t, e, 1)
		_, err = e.Decide(ctx, engine.Outcome{Kind: engine.Ignore, TriggerID: tc.id + "-a"})
		must(t, err)
	}
}

func sharedHandback(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("shared")
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		for _, id := range []string{"one", "two", "other"} {
			must(t, u.PutWork(engine.Work{ID: id}))
		}
		return u.PutThread(engine.Thread{Key: engine.ThreadKey(ref), Ref: ref, Subscriptions: []string{"one", "two"}})
	}))
	tr := trigger("change", "signal", ref)
	tr.Human = true
	receive(t, e, tr, engine.Stored)
	_, err := e.Handback(ctx, "other", tr.ID, "无关")
	checkCode(t, err, engine.CodeNotReached)
	if _, err = e.Handback(ctx, "two", tr.ID, strings.Repeat("字", 301)); err == nil {
		t.Fatal("接受了过长退回理由")
	}
	must(t, e.MarkUnrelated(ctx, "one", ref, "交给另一件工作"))
	if len(getThread(t, s, ref).Pending) != 0 {
		t.Fatal("尚有执行方时提前重排")
	}
	ok, err := e.Handback(ctx, "two", tr.ID, "单独研判")
	must(t, err)
	state := readIntake(t, s, tr.ID, []string{engine.ThreadKey(ref)}, nil)
	if !ok || len(state.disposition.Handbacks) != 2 || state.disposition.Handbacks[0].Reason != "交给另一件工作" || !slices.Equal(state.threads[engine.ThreadKey(ref)].Subscriptions, []string{"two"}) {
		t.Fatalf("共同认领的退回 %+v", state)
	}
}

func originSubscription(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	main, side := item("main"), item("side")
	receive(t, e, trigger("start", "assign", main, side), engine.Stored)
	d, err := e.Decide(ctx, engine.Outcome{Kind: engine.Accept, TriggerID: "start"})
	must(t, err)
	// 被一并订阅的次要对象，可能后来才由另一条自动事件建立 own Follow。
	tr := trigger("extra", "signal", side)
	receive(t, e, tr, engine.Stored)
	_, err = e.Reevaluate(ctx, tr.ID)
	must(t, err)
	checkDue(t, e, 1)
	_, err = e.Attach(ctx, d.WorkID, tr.ID, "一起处理")
	must(t, err)
	ok, err := e.Handback(ctx, d.WorkID, tr.ID, "另行处理")
	must(t, err)
	if !ok || !slices.Contains(getThread(t, s, side).Subscriptions, d.WorkID) || len(readWork(t, s, d.WorkID).Follows) != 1 {
		t.Fatal("撤销自动 Follow 时丢失原有订阅")
	}
}

func returnAttachedSignal(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	r := engine.NewThreadRef("feed", "stream", "updates")
	w, err := e.RequestItem(ctx, item("task"), "核对状态")
	must(t, err)
	receive(t, e, trigger("routed", "signal", r), engine.Stored)
	checkDue(t, e, 1)
	_, err = e.Attach(ctx, w.ID, "routed", "")
	must(t, err)
	receive(t, e, trigger("broadcast", "signal", r), engine.Stored)
	for _, tc := range []struct {
		id   string
		want bool
	}{{"broadcast", false}, {"routed", true}} {
		queued, err := e.Handback(ctx, w.ID, tc.id, "另行研判")
		must(t, err)
		if queued != tc.want {
			t.Fatalf("事件 %q 重排 %v，期望 %v", tc.id, queued, tc.want)
		}
	}
}
