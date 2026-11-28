package outboundtest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/pkg/protocol"
)

type blockingExtension struct {
	extension
	started chan struct{}
	finish  chan struct{}
}

func (e *blockingExtension) Execute(ctx context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	if !p.Preview {
		close(e.started)
		select {
		case <-e.finish:
		case <-ctx.Done():
			return outbound.Reply{}, ctx.Err()
		}
	}
	return e.extension.Execute(ctx, p)
}

func concurrent(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	e := &blockingExtension{started: make(chan struct{}), finish: make(chan struct{})}
	cfg := outbound.Config{Outbox: box, Executor: e, Clock: func() time.Time { return time.Unix(10, 0) }, Rate: outbound.RateLimit{Max: 1, Window: time.Minute}, Actions: []outbound.Action{{ID: "a", Level: outbound.Auto}}}
	s, err := outbound.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Act(ctx, request("a", "same"))
		done <- err
	}()
	// 等扩展真正开始发送，再用第二个服务重放和竞争最后一个窗口名额。
	select {
	case <-e.started:
	case err := <-done:
		t.Fatalf("未进入扩展 %v", err)
	}
	defer func() {
		close(e.finish)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	e2 := &extension{}
	cfg.Executor = e2
	other, err := outbound.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Act(ctx, request("a", "same")); err == nil {
		t.Fatal("把执行中报告为成功")
	}
	if _, err := other.Act(ctx, request("a", "different")); !errors.Is(err, outbound.NeedsConfirmation) {
		t.Fatal(err)
	}
	if len(e2.calls) != 1 || !e2.calls[0].Preview || len(e2.verify) != 0 {
		t.Fatalf("与在途调用竞争 %+v", e2)
	}
}

type recheckingExtension struct {
	extension
	onPreview func()
}

func (e *recheckingExtension) Execute(ctx context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	if p.Preview && e.onPreview != nil {
		e.onPreview()
	}
	return e.extension.Execute(ctx, p)
}

func staleConfirmation(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	e := &recheckingExtension{extension: extension{preview: outbound.Reply{Summary: "check", Target: "one"}}}
	cfg := outbound.Config{Outbox: box, Executor: e, Clock: func() time.Time { return time.Unix(10, 0) }, Rate: outbound.RateLimit{Max: 1, Window: time.Minute}}
	s, err := outbound.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	q := request("c", "k")
	if _, err := s.Act(ctx, q); !errors.Is(err, outbound.NeedsConfirmation) {
		t.Fatal(err)
	}
	// 旧确认的重查尚未返回，另一进程已把同一待办收严为必须点名。
	e.onPreview = func() {
		cfg.Organization = map[string]outbound.Level{"c": outbound.ConfirmNamed}
		cfg.Executor = &extension{preview: e.preview}
		other, err := outbound.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := other.Act(ctx, q); !errors.Is(err, outbound.NeedsConfirmation) {
			t.Fatal(err)
		}
	}
	if _, err := s.Confirm(ctx, "k", ""); !errors.Is(err, outbound.ErrConflict) {
		t.Fatalf("过期确认 %v", err)
	}
	for _, p := range e.calls {
		if !p.Preview {
			t.Fatal("过期预览触发了执行")
		}
	}
}

// 延迟进入仓储，重现请求准备顺序与实际取得写锁顺序相反的情况。
type delayedAdmission struct {
	outbound.Outbox
	before func()
}

func (b delayedAdmission) Save(ctx context.Context, r *outbound.Result, rate outbound.RateLimit, clock func() time.Time) error {
	if r.ID == "early" && r.State == outbound.Executing {
		b.before()
	}
	return b.Outbox.Save(ctx, r, rate, clock)
}

func queuedWindow(t *testing.T, box outbound.Outbox) {
	ctx := t.Context()
	now := time.Unix(10, 0)
	e := &extension{}
	cfg := outbound.Config{Outbox: box, Executor: e, Clock: func() time.Time { return now }, Rate: outbound.RateLimit{Max: 1, Window: time.Minute}, Actions: []outbound.Action{{ID: "a", Level: outbound.Auto}}}
	other, err := outbound.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Outbox = delayedAdmission{box, func() {
		now = now.Add(time.Hour)
		if _, err := other.Act(ctx, request("a", "later")); err != nil {
			t.Fatal(err)
		}
	}}
	early, err := outbound.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := early.Act(ctx, request("a", "early")); !errors.Is(err, outbound.NeedsConfirmation) {
		t.Fatalf("迟获写锁的请求绕过窗口: %v", err)
	}
}
