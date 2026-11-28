package app

import (
	"context"
	"encoding/json"

	"slices"

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
	return func(ctx context.Context, method string, raw json.RawMessage) (any, error) {
		call, err := src.scope.Decode(method, raw)
		if err != nil {
			return nil, err
		}
		return h.dispatch(ctx, method, call)
	}, nil
}

func (h extensionRoutes) dispatch(ctx context.Context, method string, call any) (any, error) {
	switch p := call.(type) {
	case *protocol.Trigger:
		return h.receive(ctx, *p)
	case *exthost.Describe:
		facts := make(map[string]*engine.Fact, len(p.Facts))
		for k, v := range p.Facts {
			facts[k] = (*engine.Fact)(v)
		}
		return nil, h.runtime.Commands.DescribeThread(ctx, engineRef(*p.Ref), p.Label, p.URL, facts)
	case *exthost.Patch:
		return nil, h.runtime.Commands.PatchThreadMeta(ctx, p.Key, p.Meta)
	case *exthost.Closed:
		return nil, h.runtime.Commands.ThreadClosed(ctx, engineRef(p.Thread), p.Outcome, p.At, p.Detail)
	case *exthost.Reopened:
		return nil, h.runtime.Commands.ThreadReopened(ctx, engineRef(p.Thread), p.At)
	case *exthost.Bind:
		return h.runtime.Commands.BindThread(ctx, p.WorkID, engineRef(p.Thread), *p.Own)
	case *exthost.GetThread:
		return h.get(ctx, p.Key)
	case *exthost.Subscribed:
		return h.subscribed(ctx, p.Kind)
	case *exthost.SetState:
		return h.stateSet(ctx, p)
	case *exthost.RecentReceipts:
		return h.recentReceipts(ctx, p)
	case *exthost.GetState:
		return h.stateGet(ctx, p.Key)
	case *exthost.HealthReport:
		h.health.report(h.id, p.Entry, HealthEntry{p.State, p.Detail})
		return nil, nil
	}
	switch method {
	case "state/keys":
		return h.stateKeys(ctx)
	case "work/open":
		return h.open(ctx)
	}
	return nil, exthost.ErrMethodNotFound
}
