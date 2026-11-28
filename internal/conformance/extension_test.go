package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "--craft-conformance-helper" {
		f := sampleExtension{mode: os.Args[2]}
		ready := make(chan struct{})
		f.conn = exthost.NewConn(os.Stdin, os.Stdout, func(ctx context.Context, method string, p json.RawMessage) (any, error) {
			<-ready
			return f.handle(ctx, method, p)
		}, 0)
		close(ready)
		<-f.conn.Done()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type sampleExtension struct {
	conn       *exthost.Conn
	mode, base string
	runs       int
}

func (f *sampleExtension) handle(ctx context.Context, method string, p json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var init protocol.InitializeParams
		if err := json.Unmarshal(p, &init); err != nil {
			return nil, err
		}
		if f.mode == "timeout" {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if init.Protocol != 1 && f.mode != "accept-major" {
			return map[string]bool{"ok": false}, nil
		}
		if st, err := os.Stat(init.StateDir); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("stateDir %q 无效", init.StateDir)
		}
		var config struct {
			URLs []string `json:"urls"`
		}
		if err := json.Unmarshal(init.Config, &config); err != nil {
			return nil, err
		}
		if len(config.URLs) > 0 {
			f.base = config.URLs[0]
			if !strings.HasPrefix(f.base, "http://127.0.0.1:") {
				return nil, fmt.Errorf("重放地址 %q", f.base)
			}
		}
		if f.mode == "init-foreign" {
			trigger := protocol.Trigger{ID: "early", At: instant.Format(time.RFC3339), Source: "feed", Event: "change", Threads: []protocol.ThreadRef{{Source: "other", Kind: "item", ID: "x"}}}
			err := f.call(ctx, "watch/receive", trigger)
			if rpc, ok := errors.AsType[*protocol.Error](err); !ok || rpc.Code != -32602 {
				return nil, fmt.Errorf("握手期越权调用返回 %v", err)
			}
		}
		if f.mode == "init-window" {
			resp, err := http.Get(f.base + "init?since=2000-01-01")
			if err != nil {
				return nil, err
			}
			if err := resp.Body.Close(); err != nil {
				return nil, err
			}
		}
		return protocol.InitializeResult{OK: true, Capabilities: json.RawMessage(`{}`)}, nil
	case "alarm/run":
		var a protocol.AlarmRunParams
		if err := json.Unmarshal(p, &a); err != nil {
			return nil, err
		}
		return nil, f.alarm(ctx, a)
	case "shutdown":
		os.Exit(0)
	}
	return nil, exthost.ErrMethodNotFound
}

func (f *sampleExtension) alarm(ctx context.Context, a protocol.AlarmRunParams) error {
	path := a.AlarmID
	cursorKey := "cursor"
	if f.mode == "private-cursor" {
		cursorKey = "position/stream"
	}
	if path == "items" {
		f.runs++
		var stored int
		if err := f.conn.Call(ctx, "state/get", map[string]string{"key": cursorKey}, &stored); err != nil {
			return err
		}
		if stored != f.runs-1 {
			return fmt.Errorf("第 %d 次运行读到游标 %d", f.runs, stored)
		}
		switch f.mode {
		case "crash":
			os.Exit(9)
		case "invalid", "nan":
			if _, err := fmt.Fprintln(os.Stdout, `{"jsonrpc":"2.0","method":"invalid","params":{"value":NaN}}`); err != nil {
				return err
			}
		case "long":
			if _, err := io.WriteString(os.Stdout, strings.Repeat("x", 17<<20)); err != nil {
				return err
			}
		case "unmatched":
			resp, err := http.Get(f.base + "missing")
			if err != nil {
				return err
			}
			if err = resp.Body.Close(); err != nil {
				return err
			}
			if resp.StatusCode != 599 {
				return errors.New("缺少 599")
			}
		}
		start := time.UnixMilli(a.ScheduledAtMs).UTC().Add(-7 * 24 * time.Hour)
		if f.mode == "window" {
			start = start.Add(-time.Second)
		}
		path += "?since=" + url.QueryEscape(start.Format(time.RFC3339))
	}
	probe := a.AlarmID == "items" && f.runs == 3 && a.Reason == "requested"
	if probe && f.mode == "short" {
		ctx = context.WithoutCancel(ctx)
	}
	requestCtx := ctx
	if probe && f.mode == "cancel" {
		requestCtx = context.WithoutCancel(ctx)
	}
	req, err := http.NewRequestWithContext(requestCtx, "GET", f.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if probe && f.mode != "short" {
		// 长场景在取得第一页后等待后续对象；违规版本忽略取消，只有进程回收能结束它。
		if f.mode == "cancel" {
			<-f.conn.Done()
		} else {
			<-ctx.Done()
		}
		return ctx.Err()
	}
	if resp.StatusCode >= 400 {
		return f.failure(ctx, resp.StatusCode)
	}
	if resp.Header.Get("X-Recorded") != "yes" {
		return errors.New("缺录制头")
	}
	var objects []string
	if err := json.NewDecoder(resp.Body).Decode(&objects); err != nil {
		return err
	}
	cursor := func() error { return f.call(ctx, "state/set", map[string]any{"key": cursorKey, "value": f.runs}) }
	if f.mode == "cursor" {
		if err := cursor(); err != nil {
			return err
		}
	}
	for _, id := range objects {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := f.object(ctx, id); err != nil {
			return err
		}
	}
	if f.mode != "cursor" {
		return cursor()
	}
	return nil
}

func (f *sampleExtension) call(ctx context.Context, method string, v any) error {
	return f.conn.Call(ctx, method, v, nil)
}

func (f *sampleExtension) object(ctx context.Context, id string) error {
	ref := protocol.ThreadRef{Source: "feed", Kind: "item", ID: id}
	if f.mode == "identity" {
		ref.ID = "wrong-" + id
	}
	t := protocol.Trigger{ID: id, Source: "feed", Event: "change", At: instant.Format(time.RFC3339), Threads: []protocol.ThreadRef{ref}}
	if f.mode == "unstable" {
		t.ID += fmt.Sprint(f.runs)
	}
	if f.mode == "foreign" {
		t.Threads = append(t.Threads, protocol.ThreadRef{Source: "other", Kind: "item", ID: id})
	}
	describe := func() error {
		facts := map[string]any{"title": map[string]string{"value": "标题"}, "due": map[string]string{"value": "2026-01-16"}, "urgency": map[string]string{"value": "high"}, "state": map[string]string{"value": "closed"}}
		if f.mode == "facts" {
			facts["urgency"] = map[string]string{"value": "other"}
		}
		if f.mode == "flat" {
			err := f.call(ctx, "thread/describe", map[string]any{"ref": ref, "facts": map[string]string{"title": "flat"}})
			if rpc, ok := errors.AsType[*protocol.Error](err); !ok || rpc.Code != -32602 {
				return fmt.Errorf("扁平 facts 应拒绝: %v", err)
			}
		}
		return f.call(ctx, "thread/describe", map[string]any{"ref": ref, "facts": facts})
	}
	closeThread := func() error {
		return f.call(ctx, "thread/closed", map[string]any{"thread": ref, "at": t.At, "outcome": "done"})
	}
	if f.mode != "order" && f.mode != "late-describe" {
		if err := describe(); err != nil {
			return err
		}
		if err := closeThread(); err != nil {
			return err
		}
	}
	if f.mode == "reads" {
		if err := f.readBack(ctx, ref); err != nil {
			return err
		}
	}
	var disposition string
	err := f.conn.Call(ctx, "watch/receive", t, &disposition)
	if f.mode == "foreign" {
		rpc, ok := errors.AsType[*protocol.Error](err)
		if !ok || rpc.Code != -32602 {
			return fmt.Errorf("跨源调用返回 %v", err)
		}
		// 被拒绝的越权尝试和随后合法的落盘、游标推进是独立行为。
		t.Threads = t.Threads[:1]
		if err := f.conn.Call(ctx, "watch/receive", t, &disposition); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	want := "queued"
	if f.mode == "short" && f.runs == 3 {
		want = "duplicate"
	}
	if disposition != want {
		return fmt.Errorf("重放期望 %s，得到 %s", want, disposition)
	}
	if f.mode == "late-describe" {
		return describe()
	}
	if f.mode == "order" {
		if err := describe(); err != nil {
			return err
		}
		return closeThread()
	}
	return nil
}

func (f *sampleExtension) failure(ctx context.Context, status int) error {
	report := func(state string) error {
		return f.call(ctx, "health/report", map[string]string{"entry": "upstream", "state": state})
	}
	switch status {
	case 401:
		if f.mode == "close" {
			if err := f.call(ctx, "thread/closed", map[string]any{"thread": protocol.ThreadRef{Source: "feed", Kind: "item", ID: "obj:0"}, "outcome": "closed", "at": instant.Format(time.RFC3339)}); err != nil {
				return err
			}
		}
		if f.mode != "auth" {
			return report("paused")
		}
	case 429:
		if f.mode == "bad-block" {
			return f.call(ctx, "state/set", map[string]any{"key": "blocked_until", "value": false})
		}
		if f.mode != "limit" {
			return f.call(ctx, "state/set", map[string]any{"key": "blocked_until", "value": instant.Add(time.Hour)})
		}
	case 503:
		if f.mode != "down" {
			if err := report("error"); err != nil {
				return err
			}
			return errors.New("上游暂不可用")
		}
	}
	return nil
}

func (f *sampleExtension) readBack(ctx context.Context, ref protocol.ThreadRef) error {
	k := key(ref)
	if err := f.call(ctx, "thread/patch", map[string]any{"key": k, "meta": map[string]string{"label": "updated"}}); err != nil {
		return err
	}
	if err := f.call(ctx, "thread/reopened", map[string]any{"thread": ref, "at": instant.Add(time.Second).Format(time.RFC3339)}); err != nil {
		return err
	}
	var doc struct {
		Ref        protocol.ThreadRef
		ReopenedAt string
		Closed     json.RawMessage
		Meta       struct {
			Label string
			Facts map[string]json.RawMessage
		}
	}
	if err := f.conn.Call(ctx, "thread/get", map[string]string{"key": k}, &doc); err != nil {
		return err
	}
	if doc.Ref != ref || doc.Meta.Label != "updated" || doc.Meta.Facts["title"] == nil || doc.Closed != nil || doc.ReopenedAt == "" {
		return fmt.Errorf("thread/get 返回 %+v", doc)
	}
	for _, method := range []string{"work/open", "thread/subscribed", "state/keys"} {
		var items []json.RawMessage
		if err := f.conn.Call(ctx, method, struct{}{}, &items); err != nil {
			return err
		}
		if items == nil {
			return fmt.Errorf("%s 期望数组", method)
		}
	}
	var receipts struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := f.conn.Call(ctx, "receipts/recent", map[string]int{"limit": 10}, &receipts); err != nil {
		return err
	}
	if receipts.Items == nil {
		return errors.New("缺少回声过滤所需的回执列表")
	}
	err := f.call(ctx, "work/bindThread", map[string]any{"workId": "missing", "thread": ref, "own": false})
	if e, ok := errors.AsType[*protocol.Error](err); !ok || e.Code != -32603 {
		return fmt.Errorf("绑定不存在的 Work 返回 %v", err)
	}

	return nil
}
