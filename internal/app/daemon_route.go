package app

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

type extensionRoutes struct {
	*SourceContext
	runtime *Runtime
	health  *daemonHealth
	sources []string
}

func newExtensionHandler(m exthost.Manifest, r *Runtime, health *daemonHealth) (exthost.Handler, error) {
	src, err := NewSourceContext(m, r.repo.db, r.Request)
	if err != nil {
		return nil, err
	}
	h := extensionRoutes{SourceContext: src, runtime: r, health: health}
	for _, s := range m.WatchSources {
		h.sources = append(h.sources, s.ID)
	}
	slices.Sort(h.sources)
	h.sources = slices.Compact(h.sources)
	routes := map[string]func(context.Context, json.RawMessage) (any, error){
		"watch/receive": h.receive, "thread/describe": h.describe, "thread/patch": h.patch,
		"thread/closed": h.closed, "thread/reopened": h.reopened, "work/bindThread": h.bind,
		"thread/get": h.get, "thread/subscribed": h.subscribed, "work/open": h.open,
		"state/get": h.stateGet, "state/set": h.stateSet, "state/keys": h.stateKeys, "health/report": h.report,
	}
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		f := routes[method]
		if f == nil {
			return nil, exthost.ErrMethodNotFound
		}
		if len(raw) == 0 {
			raw = json.RawMessage(`{}`)
		}
		if raw[0] != '{' {
			return nil, invalid("参数须为对象")
		}
		return f(ctx, raw)
	}, nil
}

func invalid(detail string) error { return fmt.Errorf("%w: %s", exthost.ErrInvalidParams, detail) }
func params(raw json.RawMessage, dst any) error {
	if err := jsonv2.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("%w: %w", exthost.ErrInvalidParams, err)
	}
	return nil
}

// 权限取自连接绑定的声明。将越权引用统一映射为 -32602，避免泄露另一来源的对象是否存在。
func (h extensionRoutes) authorize(ref engine.ThreadRef) error {
	if ref.ID() == "" || !h.kinds[[2]string{ref.Source(), ref.Kind()}] {
		return invalid("对象不属于本扩展声明的来源与类型")
	}
	return nil
}

func (h extensionRoutes) fromKey(key string) (engine.ThreadRef, error) {
	src, tail, _ := strings.Cut(key, ":")
	kind, id, _ := strings.Cut(tail, ":")
	ref := engine.NewThreadRef(src, kind, id)
	return ref, h.authorize(ref)
}

func (h extensionRoutes) receive(ctx context.Context, raw json.RawMessage) (any, error) {
	var p protocol.Trigger
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	return h.Receive(ctx, p)
}

func (h extensionRoutes) describe(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Key   *string                 `json:"key"`
		Ref   *engine.ThreadRef       `json:"ref"`
		Label *string                 `json:"label"`
		URL   *string                 `json:"url"`
		Facts map[string]*engine.Fact `json:"facts"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if (p.Key == nil) == (p.Ref == nil) {
		return nil, invalid("须指定 key、ref 之一")
	}
	if p.Key != nil {
		ref, err := h.fromKey(*p.Key)
		if err != nil {
			return nil, err
		}
		p.Ref = &ref
	} else if err := h.authorize(*p.Ref); err != nil {
		return nil, err
	}
	for _, f := range p.Facts {
		if f != nil && f.Value == nil {
			return nil, invalid("fact 缺少 value")
		}
	}
	return nil, h.runtime.Commands.DescribeThread(ctx, *p.Ref, p.Label, p.URL, p.Facts)
}

func (h extensionRoutes) patch(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Key  string                     `json:"key"`
		Meta map[string]json.RawMessage `json:"meta"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if _, err := h.fromKey(p.Key); err != nil {
		return nil, err
	}
	if p.Meta == nil {
		return nil, invalid("缺少 meta")
	}
	for k, v := range p.Meta {
		switch k {
		case "facts":
			return nil, invalid("facts 须经 describe 更新")
		case "label", "url":
			if v[0] != '"' && string(v) != "null" {
				return nil, invalid("meta 字段须为字符串或 null")
			}
		}
	}
	return nil, h.runtime.Commands.PatchThreadMeta(ctx, p.Key, p.Meta)
}

func (h extensionRoutes) closed(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Thread  engine.ThreadRef `json:"thread"`
		Outcome string           `json:"outcome"`
		At      string           `json:"at"`
		Detail  string           `json:"detail"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if err := h.checkFact(p.Thread, p.At); err != nil {
		return nil, err
	}
	if p.Outcome != "closed" && p.Outcome != "done" {
		return nil, invalid("outcome 无效")
	}
	return nil, h.runtime.Commands.ThreadClosed(ctx, p.Thread, p.Outcome, p.At, p.Detail)
}

func (h extensionRoutes) reopened(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Thread engine.ThreadRef `json:"thread"`
		At     string           `json:"at"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if err := h.checkFact(p.Thread, p.At); err != nil {
		return nil, err
	}
	return nil, h.runtime.Commands.ThreadReopened(ctx, p.Thread, p.At)
}

func (h extensionRoutes) checkFact(ref engine.ThreadRef, at string) error {
	if err := h.authorize(ref); err != nil {
		return err
	}
	if _, err := engine.IsoTime(at); err != nil {
		return invalid(err.Error())
	}
	return nil
}

func (h extensionRoutes) bind(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		WorkID string           `json:"workId"`
		Thread engine.ThreadRef `json:"thread"`
		Own    *bool            `json:"own"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if err := h.authorize(p.Thread); err != nil {
		return nil, err
	}
	if p.WorkID == "" || p.Own == nil {
		return nil, invalid("缺少 workId 或 own")
	}
	return h.runtime.Commands.BindThread(ctx, p.WorkID, p.Thread, *p.Own)
}

func (h extensionRoutes) report(_ context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		Entry  string `json:"entry"`
		State  string `json:"state"`
		Detail string `json:"detail"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if p.Entry == "" || p.State != "ok" && p.State != "error" && p.State != "paused" {
		return nil, invalid("健康条目或状态无效")
	}
	h.health.report(h.id, p.Entry, HealthEntry{p.State, p.Detail})
	return nil, nil
}
