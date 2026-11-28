package app

import (
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

const benchAt = "2026-01-02T00:00:00.000Z"

type benchScope struct{}

func (benchScope) Scope(string, string, map[string]json.RawMessage) engine.ScopeResult {
	return engine.ScopeResult{Listened: true}
}

func (benchScope) Closable(string, string) bool { return true }

func benchRef(i int) engine.ThreadRef {
	return engine.NewThreadRef("feed", "item", fmt.Sprintf("%05d", i))
}

func benchWorkID(i int) string { return fmt.Sprintf("work-%04d", i) }

func benchCheck(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}

func seedBench(b *testing.B, s engine.Store, count int) *engine.Engine {
	b.Helper()
	benchCheck(b, s.Atomic(b.Context(), func(u engine.Unit) error {
		for i := range count {
			ref := benchRef(i)
			if err := u.PutThread(engine.Thread{Ref: ref, Key: engine.ThreadKey(ref), CreatedAt: benchAt}); err != nil {
				return err
			}
		}
		for i := range 1000 {
			if err := u.PutWork(engine.Work{ID: benchWorkID(i), CreatedAt: benchAt}); err != nil {
				return err
			}
		}
		return nil
	}))
	p, err := engine.NewPolicy(engine.Settings{}, []engine.EventDef{{Source: "feed", Event: "assign", Default: engine.ModeAuto, Strength: engine.Explicit}})
	benchCheck(b, err)
	e, err := engine.New(engine.Config{Store: s, Scope: benchScope{}, Clock: func() string { return benchAt }, Policy: func() engine.Policy { return p }})
	benchCheck(b, err)
	return e
}

// BenchmarkRepositoryCommands 用同一夹具比较两种仓储。只在计时外恢复受影响的聚合，
// 保持目标对象的输入长度和在办数稳定；计时区间包含命令的实际提交和通知。
func BenchmarkRepositoryCommands(b *testing.B) {
	for _, backend := range []string{"Memory", "SQLite"} {
		for _, count := range []int{100, 10000} {
			for _, cmd := range []string{"Receive", "EvaluateDue", "DecideAccept", "ThreadClosed", "StopWork",
				"PrepareDelivery", "InputsDelivered", "Report", "Reply", "Answer"} {
				b.Run(fmt.Sprintf("%s/Threads_%d/%s", backend, count, cmd), func(b *testing.B) {
					var s engine.Store
					if backend == "SQLite" {
						s = sqliteRepository(b)
					} else {
						s = engine.NewMemory()
					}
					e := seedBench(b, s, count)
					b.ReportAllocs()
					switch cmd {
					case "Receive", "ThreadClosed", "StopWork":
						benchSubscribed(b, s, e, cmd)
					case "EvaluateDue", "DecideAccept":
						benchAssessment(b, s, e, cmd, count)
					default:
						benchAssignment(b, s, e, cmd)
					}
				})
			}
		}
	}
}

func benchSubscribed(b *testing.B, s engine.Store, e *engine.Engine, cmd string) {
	ctx := b.Context()
	ref, side := benchRef(0), benchRef(1)
	key := engine.ThreadKey(ref)
	ids := []string{benchWorkID(0), benchWorkID(1), benchWorkID(2)}
	th := engine.Thread{Ref: ref, Key: key, CreatedAt: benchAt, Subscriptions: ids}
	var works []engine.Work
	for i, id := range ids {
		w := engine.Work{ID: id, CreatedAt: benchAt, Follows: []engine.Follow{{Thread: key, Own: i == 0}}}
		if i == 0 {
			w.Assignments = []engine.Assignment{{ID: "run", State: engine.AssignmentRunning, CreatedAt: benchAt}}
			w.Execution = &engine.Execution{Session: &engine.Session{ID: "session", Host: "local"}}
		}
		works = append(works, w)
	}
	tr := engine.Trigger{Source: "feed", Event: "assign", At: benchAt, Threads: []engine.ThreadRef{ref, side}}
	n := 0
	for b.Loop() {
		b.StopTimer()
		tr.ID = strconv.Itoa(n)
		n++
		benchCheck(b, s.Atomic(ctx, func(u engine.Unit) error {
			for _, w := range works {
				if err := u.PutWork(w); err != nil {
					return err
				}
			}
			return u.PutThread(th)
		}))
		b.StartTimer()
		switch cmd {
		case "Receive":
			result, err := e.Receive(ctx, tr)
			if err != nil || result != engine.Stored {
				b.Fatalf("接收结果 %s, %v", result, err)
			}
		case "ThreadClosed":
			benchCheck(b, e.ThreadClosed(ctx, ref, "done", benchAt, ""))
		case "StopWork":
			w, err := e.StopWork(ctx, ids[0], "结束")
			if err != nil || w.Ended == nil {
				b.Fatalf("停止结果 %+v, %v", w.Ended, err)
			}
		}
	}
}

func benchAssessment(b *testing.B, s engine.Store, e *engine.Engine, cmd string, count int) {
	ctx := b.Context()
	n := 10
	if cmd == "DecideAccept" {
		n = 1
	}
	previous := ""
	for round := 0; b.Loop(); round++ {
		b.StopTimer()
		var primary string
		benchCheck(b, s.Atomic(ctx, func(u engine.Unit) error {
			// 接单每轮会新建一个 Work。下一轮先把它移出在办集。
			if previous != "" {
				if err := u.PutWork(engine.Work{ID: previous, Ended: &engine.WorkEnd{Outcome: engine.Done}}); err != nil {
					return err
				}
			}
			for i := range n {
				ref := benchRef(i * (count / 10))
				th := engine.Thread{Ref: ref, Key: engine.ThreadKey(ref), Subscriptions: []string{benchWorkID(1), benchWorkID(2)}}
				for j := range 3 {
					primary = fmt.Sprintf("event-%d-%d-%d", round, i, j)
					th.Pending = append(th.Pending, primary)
					tr := engine.Trigger{ID: primary, Source: "feed", Event: "assign", At: benchAt, Threads: []engine.ThreadRef{ref}, Text: "核对变化"}
					if err := u.PutTrigger(tr); err != nil {
						return err
					}
					if err := u.PutDisposition(primary, engine.TriggerDisposition{At: benchAt}); err != nil {
						return err
					}
				}
				if err := u.PutThread(th); err != nil {
					return err
				}
			}
			return nil
		}))
		if cmd == "EvaluateDue" {
			b.StartTimer()
		}
		got, err := e.EvaluateDue(ctx)
		if err != nil || got != n {
			b.Fatalf("处理对象 %d, %v", got, err)
		}
		if cmd == "EvaluateDue" {
			continue
		}
		var evalID string
		benchCheck(b, s.Read(ctx, func(r engine.Reader) error {
			th, err := r.GetThread(engine.ThreadKey(benchRef(0)))
			if err != nil {
				return err
			}
			if th == nil || th.Evaluating == nil {
				b.Fatal("缺少待决批次")
			}
			evalID = th.Evaluating.ID
			return nil
		}))
		b.StartTimer()
		d, err := e.Decide(ctx, engine.Outcome{Kind: engine.Accept, EvalID: evalID, TriggerID: primary})
		if err != nil || d.Kind != engine.Accept || d.WorkID == "" {
			b.Fatalf("接单结果 %+v, %v", d, err)
		}
		previous = d.WorkID
	}
}

func benchAssignment(b *testing.B, s engine.Store, e *engine.Engine, cmd string) {
	ctx := b.Context()
	const assignment = "run"
	ref := benchRef(0)
	origin := engine.Trigger{ID: "origin", Human: true, At: benchAt, Threads: []engine.ThreadRef{ref},
		Channel: &engine.ChannelRef{Source: "feed", Thread: json.RawMessage(`"room"`)}}
	w := engine.Work{ID: benchWorkID(0), CreatedAt: benchAt, Attention: &engine.Question{ID: "q"},
		Assignments: []engine.Assignment{{ID: assignment, State: engine.AssignmentRunning}},
		Origin:      engine.Origin{TriggerID: origin.ID}, Follows: []engine.Follow{{Own: true, Thread: engine.ThreadKey(ref)}}}
	for i := range 510 {
		id := fmt.Sprintf("input-%04d", i)
		in := engine.WorkInput{ID: id, TriggerID: id, Kind: "trigger", At: benchAt}
		if i < 500 {
			in.DeliveredTo, in.DeliveredAt = assignment, benchAt
			in.ReadAt = engine.ReadTime{Present: true, Time: benchAt}
		} else {
			w.Undelivered = append(w.Undelivered, id)
		}
		w.Inputs = append(w.Inputs, in)
	}
	if cmd == "InputsDelivered" {
		w.DeliveryBatch = w.Undelivered
	}
	benchCheck(b, s.Atomic(ctx, func(u engine.Unit) error { return u.PutTrigger(origin) }))
	for round := 0; b.Loop(); round++ {
		b.StopTimer()
		text := strconv.Itoa(round)
		benchCheck(b, s.Atomic(ctx, func(u engine.Unit) error {
			// 确认消息按起源事件去重，回答按正文去重，每轮都应实际提交。
			if cmd == "Reply" {
				origin.ID = "origin-" + text
				w.Origin.TriggerID = origin.ID
				if err := u.PutTrigger(origin); err != nil {
					return err
				}
			}
			return u.PutWork(w)
		}))
		b.StartTimer()
		switch cmd {
		case "PrepareDelivery":
			marker, err := e.PrepareDelivery(ctx, w.ID, assignment, w.Undelivered)
			if err != nil || marker == "" {
				b.Fatalf("准备批次 %q, %v", marker, err)
			}
		case "InputsDelivered":
			benchCheck(b, e.InputsDelivered(ctx, w.ID, assignment, w.Undelivered, true, benchAt))
		case "Report":
			got, err := e.Report(ctx, w.ID, assignment, engine.WorkerReport{State: engine.Progress, Summary: "核对进度"})
			if err != nil || got == nil {
				b.Fatalf("回报未保存, %v", err)
			}
		case "Reply":
			got, err := e.Reply(ctx, engine.ReplyRequest{Kind: engine.ReplyAcknowledgement, WorkID: w.ID, TriggerID: origin.ID, Text: "已收到"})
			if err != nil || got.Reused || got.Status != engine.ReplyPending {
				b.Fatalf("回复未入队 %+v, %v", got, err)
			}
		case "Answer":
			got, err := e.Answer(ctx, w.ID, text)
			if err != nil || len(got.Inputs) != len(w.Inputs)+1 {
				b.Fatalf("回答未送入 Work, %v", err)
			}
		}
	}
	if cmd == "InputsDelivered" {
		benchCheck(b, s.Read(ctx, func(r engine.Reader) error {
			got, err := r.GetDeliveryWindow(w.ID, w.Undelivered)
			if err == nil && (got == nil || len(got.Head) != 0 || got.Inputs[w.Undelivered[0]].DeliveredTo != assignment) {
				b.Fatal("队首批次未确认")
			}
			return err
		}))
	}
}
