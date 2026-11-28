package enginetest

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

func assignmentFixture(id string) engine.Work {
	return engine.Work{ID: id, Assignments: []engine.Assignment{{ID: "a", State: engine.AssignmentRunning, CreatedAt: now}}, Execution: &engine.Execution{Target: &engine.TargetSnapshot{ID: "target"}, Session: &engine.Session{Host: "local", ID: id}}}
}
func saveWork(t *testing.T, s engine.Store, w engine.Work) {
	t.Helper()
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error { return u.PutWork(w) }))
}

func deliveryCommands(t *testing.T, s engine.Store) {
	ctx := t.Context()
	w := assignmentFixture("w")
	w.Assignments[0].State = engine.AssignmentPending
	ids := []string{"b", "a", "c"}
	for _, id := range ids {
		w.Inputs = append(w.Inputs, engine.WorkInput{ID: id, TriggerID: id, Kind: "trigger"})
	}
	w.Undelivered = ids
	saveWork(t, s, w)
	var wakes int
	e := newEngine(t, config(&queueStore{Store: s, check: func(kind, key string, _ json.RawMessage) error {
		if kind == "delivery" && key == w.ID {
			wakes++
		}
		return nil
	}}))
	prepare := func(a string, ids []string, want string) {
		t.Helper()
		got, err := e.PrepareDelivery(ctx, w.ID, a, ids)
		must(t, err)
		if got != want {
			t.Fatalf("批次 %v 标记 %q，期望 %q", ids, got, want)
		}
	}
	prepare("a", ids, "")
	const start = "2026-01-01T23:59:59.999Z"
	must(t, e.AssignmentStarted(ctx, w.ID, "a", start))
	must(t, e.AssignmentStarted(ctx, w.ID, "a", now))
	a := readWork(t, s, w.ID).Assignments[0]
	if a.State != engine.AssignmentRunning || a.StartedAt != now || a.DeliveredAt != start || wakes != 1 {
		t.Fatalf("重复首条回执 %+v，唤醒 %d 次", a, wakes)
	}
	batch := ids[:2]
	mark := "w" + engine.Key(engine.Key("delivery-batch", "a", "b"))
	prepare("old", batch, "")
	prepare("a", ids[1:], "")
	prepare("a", batch, mark)
	prepare("a", batch, mark)
	prepare("a", ids, "")
	reason := "UNKNOWN：" + strings.Repeat("等待确认", 90) + "；请到对应会话核对"
	must(t, e.DeliveryFailed(ctx, w.ID, "a", reason))
	prepare("a", batch, "")
	failed := readWork(t, s, w.ID)
	if failed.Delivery.Error != reason || engine.WorkStage(failed).Action != "delivery" || !slices.Equal(failed.Undelivered, ids) || failed.Inputs[0].ReadAt.Present {
		t.Fatal("结果未知被记为已投递")
	}
	_, err := e.RetryWork(ctx, w.ID)
	must(t, err)
	prepare("a", batch, mark)
	must(t, e.InputsDelivered(ctx, w.ID, "a", ids[:1], true, now))
	if !slices.Equal(readWork(t, s, w.ID).Undelivered, ids) {
		t.Fatal("部分回执拆散了原批次")
	}
	must(t, e.InputsDelivered(ctx, w.ID, "a", batch, true, start))
	got := readWork(t, s, w.ID)
	if !slices.Equal(got.Undelivered, ids[2:]) || len(got.DeliveryBatch) != 0 || got.Delivery != nil {
		t.Fatalf("投递回执 %+v", got)
	}
	for _, in := range got.Inputs[:2] {
		if in.DeliveredAt != start || in.DeliveredTo != "a" || in.ReadAt != (engine.ReadTime{Present: true, Time: start}) {
			t.Fatalf("送达与已读未同时写回 %+v", in)
		}
	}
	must(t, e.InputsDelivered(ctx, w.ID, "a", batch, false, now))
	prepare("a", batch, "")
	prepare("a", ids[2:], "w"+engine.Key("c"))
	must(t, e.InputsDelivered(ctx, w.ID, "a", ids[2:], false, now))
	if readWork(t, s, w.ID).Inputs[2].ReadAt != (engine.ReadTime{Present: true}) {
		t.Fatal("送达未取应记 null")
	}
	must(t, e.InputsRead(ctx, w.ID, "a", ids))
	got = readWork(t, s, w.ID)
	if got.Inputs[0].ReadAt.Time != start || got.Inputs[2].ReadAt.Time != now {
		t.Fatal("已读回执覆盖首次读取时刻")
	}
}

func generationCallbacks(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	for _, condition := range []string{"已替换", "已结束", "工作已结束"} {
		t.Run(condition, func(t *testing.T) {
			w := assignmentFixture(condition)
			w.Undelivered = []string{"i"}
			w.Inputs = []engine.WorkInput{{ID: "i", Kind: "trigger", TriggerID: "t"}}
			switch condition {
			case "已替换":
				w.Assignments = append(w.Assignments, engine.Assignment{ID: "next", State: engine.AssignmentRunning})
			case "已结束":
				w.Assignments[0].State = engine.AssignmentEnded
			case "工作已结束":
				w.Ended = &engine.WorkEnd{}
			}
			saveWork(t, s, w)
			before := readIntake(t, s, "t", nil, []string{w.ID})
			if condition == "已替换" {
				_, err := e.Report(ctx, w.ID, "", engine.WorkerReport{State: engine.Failed, Session: w.Execution.Session})
				checkCode(t, err, engine.CodeUnknownAssignment)
			}
			must(t, e.AssignmentStarted(ctx, w.ID, "a", ""))
			mark, err := e.PrepareDelivery(ctx, w.ID, "a", w.Undelivered)
			must(t, err)
			if mark != "" {
				t.Fatal(mark)
			}
			must(t, e.InputsDelivered(ctx, w.ID, "a", w.Undelivered, true, ""))
			must(t, e.InputsRead(ctx, w.ID, "a", w.Undelivered))
			must(t, e.TurnEnded(ctx, w.ID, "a"))
			must(t, e.DeliveryFailed(ctx, w.ID, "a", "late"))
			must(t, e.AssignmentInterrupted(ctx, w.ID, "a"))
			got, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Failed})
			must(t, err)
			if got != nil || !reflect.DeepEqual(before, readIntake(t, s, "t", nil, []string{w.ID})) {
				t.Fatal("迟到回执改变了工作或事件")
			}
		})
	}
}

func reportCompletion(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	fact := &engine.ThreadClose{At: now, Outcome: "done"}
	for _, path := range []string{"Report", "TurnEnded"} {
		for _, tc := range []struct {
			name    string
			follows []engine.Follow
			out     engine.WorkOutcome
		}{
			{"空", nil, engine.Completed}, {"旁观", []engine.Follow{{Thread: "other"}}, engine.Completed}, {"未关闭", []engine.Follow{{Own: true}}, ""}, {"全部关闭", []engine.Follow{{Own: true, Closed: fact}, {Thread: "other"}}, engine.Done},
		} {
			t.Run(path+tc.name, func(t *testing.T) {
				w := assignmentFixture(path + tc.name)
				w.Follows = tc.follows
				if path == "TurnEnded" {
					w.Assignments[0].Report = &engine.Report{State: engine.Ready, At: now}
				}
				saveWork(t, s, w)
				if path == "Report" {
					_, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Ready})
					must(t, err)
				} else {
					must(t, e.TurnEnded(ctx, w.ID, "a"))
				}
				got := readWork(t, s, w.ID)
				if tc.out == "" {
					if got.Ended != nil || engine.WorkStage(got).Action != "review" {
						t.Fatal(got.Ended)
					}
				} else if got.Ended == nil || got.Ended.Outcome != tc.out || engine.WorkStage(got).Stage != "ended" {
					t.Fatal(got.Ended)
				}
			})
		}
	}
}

func reportStates(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	for _, tc := range []struct {
		state         engine.ReportState
		stage, action string
	}{{engine.Progress, "working", ""}, {engine.Waiting, "idle", ""}, {engine.Blocked, "person", "blocked"}, {engine.Failed, "person", "failed"}, {engine.ReportDropped, "ended", ""}} {
		w := assignmentFixture(string(tc.state))
		saveWork(t, s, w)
		_, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: tc.state, Summary: "进展"})
		must(t, err)
		got := readWork(t, s, w.ID)
		if engine.WorkStage(got) != (engine.Stage{Stage: tc.stage, Action: tc.action}) || got.Assignments[0].Report.Summary != "进展" || tc.state == engine.ReportDropped && got.Ended.Outcome != engine.Dropped {
			t.Fatalf("%s 阶段或报告异常", tc.state)
		}
	}
	w := assignmentFixture("long")
	saveWork(t, s, w)
	summary := strings.Repeat("查", 705)
	_, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Progress, Summary: summary})
	must(t, err)
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		return u.PutThread(engine.Thread{Key: "k", Evaluating: &engine.Evaluation{ID: "ev"}, Subscriptions: []string{w.ID}})
	}))
	card, err := e.Candidate(ctx, "ev")
	must(t, err)
	if readWork(t, s, w.ID).Assignments[0].Report.Summary != summary || card.Works[0].Assignments[0].Report.Summary != strings.Repeat("查", 600) {
		t.Fatal("报告正文或卡片长度错误")
	}
}

func interruption(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	for _, tc := range []struct {
		name                           string
		report                         engine.ReportState
		delivery, queued, human, later bool
		want                           int
	}{
		{"中途重启", engine.Progress, false, false, false, false, 2}, {"报告后新投递", engine.Blocked, false, false, false, true, 2}, {"等待", engine.Waiting, false, false, false, false, 1}, {"已交给人", engine.Ready, false, false, false, false, 1}, {"故障待核对", "", true, false, false, false, 1}, {"已排投递", "", false, true, false, false, 1}, {"人的会话", "", false, false, true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := assignmentFixture(tc.name)
			a := &w.Assignments[0]
			if tc.report != "" {
				a.Report = &engine.Report{State: tc.report, At: now}
				a.DeliveredAt = now
			}
			if tc.later {
				a.DeliveredAt = "2026-02-01T00:00:00.000Z"
			}
			if tc.delivery {
				w.Delivery = &engine.DeliveryFailure{At: now}
			}
			if tc.human {
				w.Mandate.Basis.Via = engine.ViaHumanDirect
			}
			saveWork(t, s, w)
			if tc.queued {
				must(t, s.Atomic(ctx, func(u engine.Unit) error { return u.Effects().Enqueue("delivery", w.ID, json.RawMessage(`{}`)) }))
			}
			must(t, e.AssignmentInterrupted(ctx, w.ID, "a"))
			got := readWork(t, s, w.ID)
			if len(got.Assignments) != tc.want {
				t.Fatalf("派工数量 %d", len(got.Assignments))
			}
			if tc.want == 2 && (got.Assignments[0].EndedReason != "hostRestarted" || got.Assignments[1].State != engine.AssignmentPending || len(got.Undelivered) != 1 || got.Inputs[0].Text == nil) {
				t.Fatal("未生成恢复说明或未替换派工")
			}
		})
	}
}

func assignmentEdges(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"启动", func() error { return e.AssignmentStarted(ctx, "missing", "a", "") }},
		{"准备", func() error { _, err := e.PrepareDelivery(ctx, "missing", "a", nil); return err }},
		{"送达", func() error { return e.InputsDelivered(ctx, "missing", "a", nil, false, "") }},
		{"读取", func() error { return e.InputsRead(ctx, "missing", "a", nil) }},
		{"轮末", func() error { return e.TurnEnded(ctx, "missing", "a") }},
		{"故障", func() error { return e.DeliveryFailed(ctx, "missing", "a", "原因") }},
		{"中断", func() error { return e.AssignmentInterrupted(ctx, "missing", "a") }},
		{"报告", func() error {
			_, err := e.Report(ctx, "missing", "", engine.WorkerReport{State: engine.Progress})
			return err
		}},
		{"回答", func() error { _, err := e.Answer(ctx, "missing", "答复"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) { checkCode(t, tc.call(), engine.CodeUnknownWork) })
	}
	w := assignmentFixture("legacy")
	saveWork(t, s, w)
	must(t, e.AssignmentStarted(ctx, w.ID, "a", now))
	if got := readWork(t, s, w.ID); got.Assignments[0].DeliveredAt != now {
		t.Fatal("旧运行记录无法补记首次投递")
	}
	for _, tc := range []engine.WorkerReport{{State: engine.Waiting, Session: w.Execution.Session}, {State: "unknown"}, {State: engine.Progress, Ask: "invalid"}, {State: engine.Progress, HumanAction: json.RawMessage("true")}} {
		if _, err := e.Report(ctx, w.ID, "a", tc); err == nil {
			t.Fatalf("错误报告被接受 %+v", tc)
		}
	}
	for _, path := range []string{"报告", "轮末"} {
		t.Run(path, func(t *testing.T) {
			testWork := assignmentFixture(path)
			testWork.Follows = []engine.Follow{{Own: true, Closed: &engine.ThreadClose{At: now, Outcome: "done"}}}
			testWork.Inputs = []engine.WorkInput{{Kind: "trigger", ID: "unread", TriggerID: "tr", Human: true, DeliveredTo: "a", ReadAt: engine.ReadTime{Present: true}}}
			saveWork(t, s, testWork)
			if path == "报告" {
				_, err := e.Report(ctx, path, "a", engine.WorkerReport{State: engine.Ready})
				must(t, err)
			} else {
				must(t, e.TurnEnded(ctx, path, "a"))
			}
			if readWork(t, s, path).Ended != nil {
				t.Fatal("自然结束丢失了人的未读消息")
			}
			must(t, e.InputsRead(ctx, path, "a", []string{"unread"}))
			must(t, e.TurnEnded(ctx, path, "a"))
			if got := readWork(t, s, path); got.Ended == nil || got.Ended.Outcome != engine.Done {
				t.Fatal("读取后未按关闭事实结束")
			}
		})
	}
	w = assignmentFixture("blocked")
	w.Undelivered = []string{"checking", "bot", "person"}
	w.DeliveryBatch = []string{"checking"}
	w.Inputs = []engine.WorkInput{{Kind: "trigger", ID: "checking", TriggerID: "one"}, {Kind: "trigger", ID: "bot", TriggerID: "two"}, {Kind: "trigger", ID: "person", TriggerID: "three", Human: true}}
	saveWork(t, s, w)
	_, err := e.Report(ctx, w.ID, "a", engine.WorkerReport{State: engine.Failed})
	must(t, err)
	if got := readWork(t, s, w.ID); !slices.Equal(got.Undelivered, []string{"checking", "person"}) {
		t.Fatal("失败后丢失正在核对的批次或未保留人的输入")
	}
}

func deliveryReplay(t *testing.T, s engine.Store) {
	ctx := t.Context()
	e := newEngine(t, config(s))
	w := assignmentFixture("batch")
	for i := 90; i >= 0; i-- {
		id := fmt.Sprintf("i%02d", i)
		w.Inputs = append(w.Inputs, engine.WorkInput{ID: id, Kind: "trigger", TriggerID: id})
		w.Undelivered = append(w.Undelivered, id)
	}
	saveWork(t, s, w)
	ids := slices.Clone(w.Undelivered)
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	want := "w" + engine.Key(engine.Key(append([]string{"delivery-batch"}, sorted...)...))
	var sent []string
	send := func(ids []string) {
		t.Helper()
		marker, err := e.PrepareDelivery(ctx, w.ID, "a", ids)
		must(t, err)
		if marker != "" {
			sent = append(sent, marker)
			must(t, e.InputsDelivered(ctx, w.ID, "a", ids, true, ""))
		}
	}
	send(ids)
	send(ids)
	if !slices.Equal(sent, []string{want}) {
		t.Fatalf("同一批次实际发送标记 %v", sent)
	}
}
