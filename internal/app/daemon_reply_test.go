package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

const replyThread = `{"object":9007199254740993}`

func queueWorkerReply(t *testing.T, e *engine.Engine, source, text string, kind engine.ReplyKind) engine.ReplyReceipt {
	t.Helper()
	ctx := t.Context()
	ref := engine.NewThreadRef("feed", "item", text)
	w, err := e.RequestItem(ctx, ref, "处理来信")
	checkErr(t, err)
	tr := engine.Trigger{ID: "trigger-" + text, At: benchAt, Source: "feed", Event: "change", Human: true, Threads: []engine.ThreadRef{ref},
		Channel: &engine.ChannelRef{Source: source, Thread: json.RawMessage(replyThread)}}
	_, err = e.Receive(ctx, tr)
	checkErr(t, err)
	a := w.Assignments[0].ID
	if kind == engine.ReplyReply {
		_, err = e.Report(ctx, w.ID, a, engine.WorkerReport{State: engine.Progress, Summary: text, ReplyToTriggerID: tr.ID})
		checkErr(t, err)
	}
	req := engine.ReplyRequest{WorkID: w.ID, AssignmentID: a, Kind: kind, Text: text, TriggerID: tr.ID}
	first, err := e.Reply(ctx, req)
	checkErr(t, err)
	again, err := e.Reply(ctx, req)
	checkErr(t, err)
	if !again.Reused || again.ID != first.ID {
		t.Fatal("重复命令生成了新的消息", again)
	}
	return first
}

func TestDaemonReplyEffect(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	p := installTestManifest(t, root, "reader", "feed", 1, true)
	raw, err := os.ReadFile(p)
	checkErr(t, err)
	m := strings.TrimSuffix(string(raw), "}") + `,"ports":{"channels":[{"id":"chat","replyAction":"post"}]},"actions":[{"id":"post","level":"auto"}]}`
	checkErr(t, os.WriteFile(p, []byte(m), 0600))
	clock := newTestClock()
	db, err := store.OpenSQLite(t.Context(), filepath.Join(home, "craft.db"), nil, store.WithClock(clock.Now))
	checkErr(t, err)
	t.Cleanup(func() { checkErr(t, db.Close()) })
	r := testRuntime(t, db)
	ok := queueWorkerReply(t, r.Commands, "chat", "收到", engine.ReplyAcknowledgement)
	pending := queueWorkerReply(t, r.Commands, "chat", "检查 /home/user/file", engine.ReplyReply)
	missing := queueWorkerReply(t, r.Commands, "absent", "完成", engine.ReplyReply)
	changed := make(chan struct{}, 1)
	d := NewDaemon(DaemonConfig{StateDir: home, ExtensionDirs: []string{root}, Clock: clock, OnHealth: func() {
		select {
		case changed <- struct{}{}:
		default:
		}
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	checkErr(t, d.Start(ctx))
	t.Cleanup(func() { checkErr(t, d.Stop(context.Background())) })
	for {
		h := d.Health().Effects["reply"]
		if h.MaxAttempts > 0 {
			t.Fatalf("积压的 reply 未被消费: %+v", h)
		}
		if h.Status == "ok" && h.Waiting == 0 {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			t.Fatal(d.Health())
		}
	}
	got, err := d.Outbound().Show(ctx, ok.ID)
	checkErr(t, err)
	var receipt struct {
		Key, Action string
		Params      struct {
			Thread json.RawMessage
			Body   string
		}
		Count int
	}
	checkErr(t, json.Unmarshal(got.Receipt, &receipt))
	if got.State != outbound.Done || got.SentSeq < 1 || receipt.Count != 1 || receipt.Key != ok.ID || receipt.Action != "post" || string(receipt.Params.Thread) != replyThread || receipt.Params.Body != "收到" {
		t.Fatal("扩展收到的目的地、正文、幂等键或次数有误", got)
	}
	got, err = d.Outbound().Show(ctx, pending.ID)
	checkErr(t, err)
	if got.State != outbound.Pending || got.Level != outbound.Confirm || got.Source != outbound.Worker || d.Health().Outbox.Pending != 1 {
		t.Fatal("内容命中后没有进入待确认清单", got)
	}
	if d.Health().Reports["reply"][missing.ID].Status != "error" {
		t.Fatal("无渠道的回复未进入健康告警", d.Health())
	}
	checkErr(t, db.View(ctx, func(rd store.SQLiteReader) error {
		for id, want := range map[string]engine.ReplyStatus{ok.ID: engine.ReplySent, pending.ID: engine.ReplyConfirm, missing.ID: engine.ReplyFailed} {
			status, err := (engineReader{rd}).GetReplyStatus(id)
			if err != nil {
				return err
			}
			if status != want {
				t.Errorf("消息 %s 回执 %s, 期望 %s", id, status, want)
			}
		}
		return nil
	}))
	_, err = d.Outbound().Confirm(ctx, pending.ID, "")
	checkErr(t, err)
	checkErr(t, r.repo.Read(ctx, func(rd engine.Reader) error {
		got, err := rd.GetReplyStatus(pending.ID)
		if got != engine.ReplySent {
			t.Errorf("人工确认后的消息回执 = %s", got)
		}
		return err
	}))
	checkErr(t, d.View(ctx, func(r CommandReader) error {
		items, err := r.Attention()
		if !slices.ContainsFunc(items, func(i engine.AttentionItem) bool {
			return i.Kind == "health" && i.Ref.Attention == missing.ID && strings.Contains(i.Detail, "absent")
		}) {
			t.Errorf("缺少无法路由的回复待办 %s", missing.ID)
		}
		return err
	}))
}

type replyExtension struct {
	sends      int
	sendErr    error
	previewErr error
}

func (e *replyExtension) Execute(_ context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	if p.Preview {
		return outbound.Reply{Summary: "回复"}, e.previewErr
	}
	e.sends++
	return outbound.Reply{Value: json.RawMessage(`{"ok":true}`)}, e.sendErr
}

func (*replyExtension) Verify(context.Context, protocol.ActionVerifyParams) (outbound.Verification, json.RawMessage, error) {
	return outbound.Inconclusive, nil, nil
}

func TestReplyReplay(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"sent", "timeout", "confirm", "missing", "withdrawn", "rate", "preview"} {
			t.Run(backend+"/"+mode, func(t *testing.T) { testReplyReplay(t, backend, mode) })
		}
	}
}

// 内存仓储不暴露消费接口。旁录同一事务提交的载荷，两条路径都重放引擎实际生成的行。
type replyQueue struct {
	engine.Store
	row store.Effect
}
type replyQueueUnit struct {
	engine.Unit
	queue *replyQueue
}

func (q *replyQueue) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return q.Store.Atomic(ctx, func(u engine.Unit) error { return fn(replyQueueUnit{u, q}) })
}
func (u replyQueueUnit) Effects() engine.Effects { return u }
func (u replyQueueUnit) Enqueue(kind, key string, raw json.RawMessage) error {
	if err := u.Unit.Effects().Enqueue(kind, key, raw); err != nil {
		return err
	}
	if kind == "reply" {
		u.AfterCommit(func() { u.queue.row = store.Effect{Kind: kind, Key: key, Value: raw} })
	}
	return nil
}

func replyTestHandler(t *testing.T, backend string) (*replyHandler, *replyExtension, *replyQueue) {
	t.Helper()
	repo, box := engine.NewMemory(), outbound.NewMemory()
	if backend == "sqlite" {
		db := outboxDB(t)
		repo, box = NewSQLiteStore(db), NewSQLiteOutbox(db)
	}
	queue := &replyQueue{Store: repo}
	policy, err := engine.NewPolicy(engine.Settings{}, nil)
	checkErr(t, err)
	e, err := engine.New(engine.Config{Store: queue, Scope: benchScope{}, Clock: func() string { return benchAt }, Policy: func() engine.Policy { return policy }})
	checkErr(t, err)
	ext := &replyExtension{}
	service, err := outbound.New(outbound.Config{Outbox: box, Executor: ext, Clock: newTestClock().Now,
		Rate: outbound.RateLimit{Max: 1, Window: time.Minute}, Actions: []outbound.Action{{ID: "post", Level: outbound.Auto}}})
	checkErr(t, err)
	return &replyHandler{repo: repo, commands: e, box: service, actions: map[string]string{"chat": "post"}, health: newDaemonHealth(nil)}, ext, queue
}

func testReplyReplay(t *testing.T, backend, mode string) {
	ctx := t.Context()
	h, ext, queue := replyTestHandler(t, backend)
	body, source := "收到", "chat"
	state, status, sends := outbound.Done, engine.ReplySent, 1
	switch mode {
	case "timeout":
		ext.sendErr = context.DeadlineExceeded
		state, status = outbound.Executing, engine.ReplyUnknown
	case "confirm":
		body = "内网 10.2.3.4"
		state, status, sends = outbound.Pending, engine.ReplyConfirm, 0
	case "rate":
		_, err := h.box.Act(ctx, outbound.Request{Action: "post", IdempotencyKey: "previous", Source: outbound.Worker})
		checkErr(t, err)
		state, status = outbound.Pending, engine.ReplyConfirm
	case "missing":
		source = "absent"
		state, status, sends = "", engine.ReplyFailed, 0
	case "withdrawn":
		body = "内网 10.2.3.4"
		state, status, sends = outbound.Withdrawn, engine.ReplyDropped, 0
	}
	rec := queueWorkerReply(t, h.commands, source, body, engine.ReplyAcknowledgement)
	row := queue.row
	if row.Key != rec.ID {
		t.Fatalf("reply effect 的键 %q, 期望 %q", row.Key, rec.ID)
	}
	var msg engine.OutboundMessage
	checkErr(t, json.Unmarshal(row.Value, &msg))
	// 渠道已返回、引擎尚未记录结果时中断。重启后只能消费同一条回执。
	if mode == "sent" || mode == "timeout" || mode == "confirm" {
		sent, cause := h.Send(ctx, msg)
		if sent != status || (cause != nil) != (mode == "timeout") {
			t.Fatal(sent, cause)
		}
	}
	if mode == "confirm" {
		ext.previewErr = context.DeadlineExceeded
	}
	if mode == "preview" {
		ext.previewErr = context.DeadlineExceeded
		result := h.Run(ctx, row)
		if !errors.Is(result.Err, context.DeadlineExceeded) || ext.sends != 0 {
			t.Fatal("预览失败没有交给调度器退避", result)
		}
		ext.previewErr = nil
	}
	if mode == "withdrawn" {
		checkErr(t, h.Run(ctx, row).Err)
		checkErr(t, h.repo.Atomic(ctx, func(u engine.Unit) error { return u.PutReplyStatus(rec.ID, engine.ReplyDropped) }))
		if backend == "sqlite" {
			if _, err := h.box.Confirm(ctx, rec.ID, ""); !errors.Is(err, outbound.ErrDenied) {
				t.Fatal("撤回后仍允许人工外发", err)
			}
		}
	}
	for range 2 {
		result := h.Run(ctx, row)
		if result.Err != nil || result.Later || !result.Due.IsZero() {
			t.Fatalf("已处理的回复仍要重试: %+v", result)
		}
	}
	checkErr(t, h.repo.Read(ctx, func(r engine.Reader) error {
		got, err := r.GetReplyStatus(rec.ID)
		if got != status {
			t.Errorf("回执 %s, 期望 %s", got, status)
		}
		return err
	}))
	if mode == "timeout" {
		item, err := h.box.Show(ctx, rec.ID)
		checkErr(t, err)
		if item.State != state {
			t.Fatal("超时后缺少待核实记录", item)
		}
		checkErr(t, h.box.Recover(ctx))
		state = outbound.Unknown
		checkErr(t, h.Run(ctx, row).Err)
	}
	item, err := h.box.Show(ctx, rec.ID)
	if state != "" || !errors.Is(err, outbound.ErrMissing) {
		checkErr(t, err)
	}
	if item.State != state || ext.sends != sends {
		t.Fatalf("outbox %s, 调用 %d 次", item.State, ext.sends)
	}
}
