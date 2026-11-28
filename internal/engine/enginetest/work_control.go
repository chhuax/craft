package enginetest

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func stopAtomic(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	ref, side, contextRef := item("bound"), item("side"), item("context")
	session := engine.Session{Host: "local", ID: "one"}
	keys := []string{engine.ThreadKey(ref), engine.ThreadKey(side), engine.ThreadKey(contextRef)}
	w := engine.Work{ID: "w", Execution: &engine.Execution{Session: &session}, Follows: []engine.Follow{{Thread: keys[0], Own: true}}, Assignments: []engine.Assignment{{ID: "a", State: engine.AssignmentRunning}, {ID: "b", State: engine.AssignmentPending}}}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutWork(w))
		for i, r := range []engine.ThreadRef{ref, side, contextRef} {
			th := engine.Thread{Key: keys[i], Ref: r, Subscriptions: []string{"other", "w"}}
			if i == 2 {
				th.Context = []string{"w"}
			}
			must(t, u.PutThread(th))
		}
		return nil
	}))
	fail := true
	var stops []string
	store := &queueStore{Store: s, check: func(kind, key string, value json.RawMessage) error {
		if kind == "stop" {
			var v struct {
				WorkID, AssignmentID string
				Session              engine.Session
			}
			must(t, json.Unmarshal(value, &v))
			if key != v.AssignmentID || v.WorkID != w.ID || v.Session != session {
				t.Fatalf("停止载荷 %s[%s] %s", kind, key, value)
			}
			stops = append(stops, key)
			if fail {
				return errQueue
			}
		}
		return nil
	}}
	var broadcast []engine.Event
	s.Subscribe(func(ev engine.Event) { broadcast = append(broadcast, ev) })
	e := evalEngine(t, store, &p)
	before := readIntake(t, s, "start", keys, []string{w.ID})
	_, err := e.StopWork(ctx, w.ID, "终止")
	if !errors.Is(err, errQueue) || len(broadcast) != 0 || !reflect.DeepEqual(before, readIntake(t, s, "start", keys, []string{w.ID})) {
		t.Fatal("停止请求失败后仍有事实或广播")
	}
	fail = false
	stopped, err := e.StopWork(ctx, w.ID, "终止")
	must(t, err)
	if stopped.Ended == nil || *stopped.Ended != (engine.WorkEnd{At: now, By: "human", Outcome: engine.Stopped, Reason: "终止"}) || !slices.Equal(stops, []string{"a", "a"}) {
		t.Fatalf("停止结果 %+v %v", stopped.Ended, stops)
	}
	for _, r := range []engine.ThreadRef{ref, side, contextRef} {
		th := getThread(t, s, r)
		if !slices.Equal(th.Subscriptions, []string{"other"}) || len(th.Context) != 0 {
			t.Fatal("退订影响了其他工作或留下只读订阅")
		}
	}
	prev := len(broadcast)
	_, err = e.StopWork(ctx, w.ID, "再次终止")
	must(t, err)
	if prev != len(broadcast) {
		t.Fatal("重复停止产生了事件")
	}
	tr := trigger("later", "assign", ref)
	tr.Human = true
	tr.Addressed = "direct"
	receive(t, e, tr, engine.Stored)
	checkDue(t, e, 1)
	_, err = e.Attach(ctx, w.ID, tr.ID, "")
	checkCode(t, err, engine.CodeWorkStopped)
	if readWork(t, s, w.ID).Ended == nil {
		t.Fatal("人的消息恢复了已停止的工作")
	}
	_, err = e.Handback(ctx, w.ID, tr.ID, "拒绝")
	checkCode(t, err, engine.CodeWorkEnded)
}

func workRetry(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{})
	var effects []string
	e := evalEngine(t, &queueStore{Store: s, check: func(kind, key string, _ json.RawMessage) error { effects = append(effects, kind+"/"+key); return nil }}, &p)
	ctx := t.Context()
	for _, tc := range []struct {
		name                      string
		state                     engine.AssignmentState
		report                    engine.ReportState
		delivery, ended, answered bool
		want                      string
	}{
		{"无派工", "", "", false, false, false, ""}, {"运行中", "running", "progress", false, false, false, ""}, {"已就绪", "running", "ready", false, false, false, ""},
		{"已阻塞", "running", "blocked", false, false, false, "new"}, {"已失败", "running", "failed", false, false, false, "new"},
		{"启动中已建会话后失败", "pending", "failed", false, false, false, "new"}, {"已答复", "running", "failed", false, false, true, ""},
		{"工作已结束", "running", "failed", true, true, false, ""}, {"投递失败", "running", "progress", true, false, false, "delivery"},
		{"启动失败", "pending", "", true, false, false, "launch"}, {"派工已结束且投递失败", "ended", "failed", true, false, false, ""},
		{"已重试", "pending", "", false, false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := engine.Work{ID: tc.name}
			if tc.state != "" {
				a := engine.Assignment{ID: "old", State: tc.state}
				if tc.report != "" {
					a.Report = &engine.Report{State: tc.report, At: "2025-12-31T00:00:00.000Z"}
				}
				w.Assignments = []engine.Assignment{a}
			}
			if tc.ended {
				w.Ended = &engine.WorkEnd{By: "human"}
			}
			if tc.delivery {
				w.Delivery = &engine.DeliveryFailure{Error: "offline"}
			}
			w.Inputs = []engine.WorkInput{{ID: "unread", Kind: "trigger", TriggerID: "tr", ReadAt: engine.ReadTime{Present: true}, DeliveredTo: "old"}, {ID: "legacy", Kind: "trigger", TriggerID: "legacy", DeliveredTo: "old"}}
			if tc.answered {
				w.Inputs = append(w.Inputs, engine.WorkInput{ID: "human", Kind: "trigger", TriggerID: "human", Human: true, At: now})
			}
			w.Inputs = append(w.Inputs, engine.WorkInput{ID: "next", Kind: "trigger", TriggerID: "next"})
			w.Undelivered = []string{"next"}
			w.Execution = &engine.Execution{Session: &engine.Session{ID: "same", Host: "local"}}
			must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(w) }))
			effects = nil
			got, err := e.RetryWork(ctx, w.ID)
			if tc.want == "" {
				checkCode(t, err, engine.CodeNotRetryable)
				if len(effects) != 0 {
					t.Fatal(effects)
				}
				return
			}
			must(t, err)
			if got.Delivery != nil || !reflect.DeepEqual(got.Execution, w.Execution) {
				t.Fatal("重试未清故障或会话改变")
			}
			switch tc.want {
			case "new":
				if len(got.Assignments) != 2 || got.Assignments[0].State != engine.AssignmentEnded || got.Assignments[1].State != engine.AssignmentPending || !slices.Equal(effects, []string{"stop/old", "launch/" + got.Assignments[1].ID}) || !slices.Equal(got.Undelivered, []string{"unread", "next"}) {
					t.Fatalf("派工重试 %v %v %v", got.Assignments, effects, got.Undelivered)
				}
				_, err = e.RetryWork(ctx, w.ID)
				checkCode(t, err, engine.CodeNotRetryable)
			case "launch", "delivery":
				key := w.ID
				if tc.want == "launch" {
					key = "old"
				}
				if len(got.Assignments) != 1 || !slices.Equal(effects, []string{tc.want + "/" + key}) {
					t.Fatalf("投递重试 %v %v", got.Assignments, effects)
				}
			}
		})
	}
}

func humanDecisions(t *testing.T, s engine.Store) {
	p := evalPolicy(t, engine.Settings{Paused: true})
	e := evalEngine(t, s, &p)
	ctx := t.Context()
	r := item("requested")
	w, err := e.RequestItem(ctx, r, "处理该对象")
	must(t, err)
	state := readIntake(t, s, w.Origin.TriggerID, []string{engine.ThreadKey(r)}, []string{w.ID})
	tr := state.trigger
	if tr == nil || tr.Source != "craft" || tr.Event != "person.request" || !tr.Human || tr.Text != "处理该对象" || w.Mandate.Strength != engine.Explicit || w.Mandate.Basis.Via != engine.ViaPersonRequest || len(w.Assignments) != 1 || !w.Follows[0].Own || state.disposition.DecisionID != w.DecisionID {
		t.Fatalf("人的授权 %+v %+v", w, tr)
	}
	again, err := e.RequestItem(ctx, r, "重复")
	must(t, err)
	if again.ID != w.ID {
		t.Fatal("为在办对象重复建单")
	}
	_, err = e.Dismiss(ctx, w.DecisionID)
	checkCode(t, err, engine.CodeUnknownProblem)
	_, err = e.Dismiss(ctx, "missing")
	checkCode(t, err, engine.CodeUnknownProblem)
	d := engine.Decision{ID: "problem", ThreadKey: engine.ThreadKey(r), TriggerIDs: []string{"failed"}, Kind: engine.Unavailable}
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutDecision(d) }))
	d, err = e.Dismiss(ctx, d.ID)
	must(t, err)
	if !d.Dismissed || len(engine.Attention(engine.AttentionSet{Threads: []engine.Thread{getThread(t, s, r)}, Decisions: []engine.Decision{d}})) != 0 {
		t.Fatal("失败待办未消失")
	}
	must(t, e.ThreadClosed(ctx, item("closed"), "done", now, ""))
	_, err = e.RequestItem(ctx, item("closed"), "已关闭")
	checkCode(t, err, engine.CodeNotAdmissible)
}

func requestLifecycle(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{Paused: true})
	ref := item("request")
	key := engine.ThreadKey(ref)
	fail := true
	e := evalEngine(t, &queueStore{Store: s, check: func(kind, _ string, _ json.RawMessage) error {
		if fail && kind == "launch" {
			return errQueue
		}
		return nil
	}}, &p)
	must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.PutWork(engine.Work{ID: "reader"}) }))
	_, err := e.FollowForContext(ctx, "reader", ref, "关注进展")
	must(t, err)
	before := readIntake(t, s, engine.Key("request", key, now), []string{key}, []string{"reader"})
	_, err = e.RequestItem(ctx, ref, "请处理")
	if !errors.Is(err, errQueue) || !reflect.DeepEqual(before, readIntake(t, s, engine.Key("request", key, now), []string{key}, []string{"reader"})) {
		t.Fatal("人工请求未整体回滚")
	}
	fail = false
	w, err := e.RequestItem(ctx, ref, "请处理")
	must(t, err)
	reader := readWork(t, s, "reader")
	if len(reader.Follows) != 0 || len(reader.Inputs) != 1 || !reader.Inputs[0].Context || reader.Inputs[0].TriggerID != w.Origin.TriggerID {
		t.Fatal("只读订阅误认领了人工请求")
	}
	_, err = e.StopWork(ctx, w.ID, "重新授权")
	must(t, err)
	next, err := e.RequestItem(ctx, ref, "新的范围")
	must(t, err)
	if next.ID == w.ID || next.Mandate.Scope != "新的范围" || next.Origin.TriggerID == w.Origin.TriggerID || readWork(t, s, w.ID).Ended == nil {
		t.Fatal("同毫秒人工再发起覆盖了旧授权")
	}
}

func requestAlreadyPending(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	for _, active := range []bool{false, true} {
		name := "pending"
		if active {
			name = "active"
		}
		ref := item(name)
		tr := trigger(name, "person.request", ref)
		tr.Source = "craft"
		tr.Human = true
		tr.Addressed = "direct"
		tr.Text = "处理"
		receive(t, e, tr, engine.Stored)
		receive(t, e, trigger(name+"-other", "signal", ref), engine.Stored)
		if active {
			checkDue(t, e, 1)
		}
		w, err := e.RequestItem(ctx, ref, "重复请求")
		must(t, err)
		th := getThread(t, s, ref)
		if w.Origin.TriggerID != tr.ID || th.Evaluating != nil || !slices.Equal(th.Pending, []string{name + "-other"}) {
			t.Fatalf("重复发起改变了已有请求或吃掉其他事件 %+v %+v", w.Origin, th)
		}
		// 清掉本组待研判事件，隔离下一组的批次计数。
		_, err = e.Decide(ctx, engine.Outcome{Kind: engine.Ignore, TriggerID: name + "-other"})
		must(t, err)
	}
}

func requestUnsubscribedOrigin(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	e := evalEngine(t, s, &p)
	ref := item("origin")
	w, err := e.RequestItem(ctx, ref, "处理")
	must(t, err)
	_, err = e.Unsubscribe(ctx, engine.ThreadKey(ref))
	must(t, err)
	again, err := e.RequestItem(ctx, ref, "再次发起")
	must(t, err)
	if w.ID != again.ID || len(again.Assignments) != 1 {
		t.Fatal("退订后重复受理相同起源")
	}
}

// 数据出现「运行中却没有登记会话」的不一致时，人必须仍能停止这个 Work：
// 没有会话就没有要停的东西，不能因此报错把人挡住。
func stopWithoutSession(t *testing.T, s engine.Store) {
	ctx := t.Context()
	p := evalPolicy(t, engine.Settings{})
	ref := item("nosession")
	key := engine.ThreadKey(ref)
	w := engine.Work{ID: "w", Assignments: []engine.Assignment{{ID: "a", State: engine.AssignmentRunning}}}
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		must(t, u.PutWork(w))
		return u.PutThread(engine.Thread{Key: key, Ref: ref, Subscriptions: []string{"w"}})
	}))
	var stops int
	store := &queueStore{Store: s, check: func(kind, _ string, _ json.RawMessage) error {
		if kind == "stop" {
			stops++
		}
		return nil
	}}
	e := evalEngine(t, store, &p)
	stopped, err := e.StopWork(ctx, w.ID, "终止")
	must(t, err)
	if stopped.Ended == nil || stopped.Ended.By != "human" || stops != 0 {
		t.Fatalf("没有会话时的停止 %+v，stop 入队 %d 次", stopped.Ended, stops)
	}
}

func stopRegistered(t *testing.T, s engine.Store) {
	ctx := t.Context()
	for _, tc := range []struct {
		name  string
		state engine.AssignmentState
		owner string
		want  bool
	}{
		{"首条未确认", engine.AssignmentPending, "current", true},
		{"未创建", engine.AssignmentPending, "", false},
		{"沿用前代位置尚未登记", engine.AssignmentPending, "old", false},
		{"已经启动", engine.AssignmentRunning, "current", true},
		{"已经结束", engine.AssignmentEnded, "current", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := engine.Work{ID: tc.name, Assignments: []engine.Assignment{{ID: "old", State: engine.AssignmentPending}, {ID: "current", State: tc.state}}}
			session := engine.Session{Host: "local", ID: tc.name}
			if tc.owner != "" {
				w.Execution = &engine.Execution{Session: &session, Assignment: tc.owner}
			}
			saveWork(t, s, w)
			var stops int
			fail := tc.want
			e := newEngine(t, config(&queueStore{Store: s, check: func(kind, key string, raw json.RawMessage) error {
				if kind != "stop" {
					return nil
				}
				var payload struct {
					WorkID, AssignmentID string
					Session              engine.Session
				}
				must(t, json.Unmarshal(raw, &payload))
				if key != "current" || payload.AssignmentID != key || payload.WorkID != w.ID || payload.Session != session {
					t.Fatalf("停止了错误的派工或会话 %s %s", key, raw)
				}
				if fail {
					return errQueue
				}
				stops++
				return nil
			}}))
			if fail {
				before := readWork(t, s, w.ID)
				_, err := e.StopWork(ctx, w.ID, "终止")
				if !errors.Is(err, errQueue) || !reflect.DeepEqual(before, readWork(t, s, w.ID)) {
					t.Fatal("入队失败没有整体回滚", err)
				}
				fail = false
			}
			for range 2 {
				stopped, err := e.StopWork(ctx, w.ID, "终止")
				must(t, err)
				if stopped.Ended == nil {
					t.Fatal("工作没有结束")
				}
			}
			if (tc.want && stops != 1) || (!tc.want && stops != 0) {
				t.Fatalf("stop 入队 %d 次", stops)
			}
		})
	}
}
