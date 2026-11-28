package cli

import (
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"strings"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/pkg/protocol"
)

type actionParams struct {
	Action         string          `json:"action,omitempty"`
	Params         json.RawMessage `json:"params,omitzero"`
	ID             string          `json:"id,omitempty"`
	IdempotencyKey string          `json:"idempotencyKey,omitempty"`
	NamedTarget    string          `json:"namedTarget,omitempty"`
	Yes            bool            `json:"yes,omitempty"`
	Source         outbound.Source `json:"source,omitempty"`
}

func outboundCommand(i *invocation) (result, error) {
	_, yes := i.flags["--yes"]
	p := actionParams{Yes: yes, IdempotencyKey: i.flags["--key"]}
	if i.binding != nil {
		// 绑定只来自进程入口。即使调用者带 --yes，也必须把后台语义传到 daemon。
		p.Source = outbound.Worker
	}
	if i.spec.path == "act" {
		p.Action, p.Params = i.args[0], json.RawMessage(i.flags["--params"])
		if !jsontext.Value(p.Params).IsValid() {
			return result{}, invalid("--params 需要有效 JSON")
		}
	} else {
		p.ID = i.args[0]
	}
	st, _, err := ensureFresh(i)
	if err != nil {
		return result{}, err
	}
	raw, err := st.call(i.ctx, strings.ReplaceAll(i.spec.path, " ", "."), p)
	return result{payload: raw}, err
}

func (a *localAPI) action(ctx context.Context, req protocol.Request) (any, error) {
	var p actionParams
	if req.Params != nil {
		if len(*req.Params) == 0 || (*req.Params)[0] != '{' || jsonv2.Unmarshal(*req.Params, &p) != nil {
			return nil, rpcError(-32602, "动作参数无效")
		}
	}
	// 已鉴权的本地接口默认允许会话操作。来源标记只能收紧该权限。
	if p.Source == "" {
		p.Source = outbound.Session
	}
	if p.Source != outbound.Worker && p.Source != outbound.Session {
		return nil, rpcError(-32602, "动作来源无效")
	}
	if p.Source == outbound.Worker && req.Method != "act" {
		return nil, rpcError(protocol.ActionDenied, "后台会话不能管理外发待办")
	}
	s := a.daemon.Outbound()
	switch req.Method {
	case "act":
		return p.act(ctx, s)
	case "outbox.list":
		items, err := s.List(ctx)
		return map[string]any{"items": items}, err
	}
	if p.ID == "" {
		return nil, rpcError(-32602, "需要外发记录 ID")
	}
	switch req.Method {
	case "outbox.show":
		return actionResult(s.Show(ctx, p.ID))
	case "outbox.withdraw":
		if err := s.Withdraw(ctx, p.ID); err != nil {
			return actionResult(outbound.Result{}, err)
		}
		return map[string]string{"id": p.ID, "state": string(outbound.Withdrawn)}, nil
	case "outbox.confirm":
		return p.confirm(ctx, s)
	default:
		return nil, rpcError(-32601, "方法不存在")
	}
}

func (p actionParams) act(ctx context.Context, s *outbound.Service) (any, error) {
	if p.Action == "" || p.Params == nil {
		return nil, rpcError(-32602, "需要动作与 params")
	}
	if p.IdempotencyKey == "" {
		wire := jsontext.Value(p.Params).Clone()
		if err := wire.Format(jsontext.ReorderRawObjects(true)); err != nil {
			return nil, rpcError(-32602, "动作参数不是有效 JSON")
		}
		p.IdempotencyKey = engine.Key(p.Action, string(wire))
	}
	return actionResult(s.Act(ctx, outbound.Request{Action: p.Action, Params: p.Params, IdempotencyKey: p.IdempotencyKey, Yes: p.Yes, NamedTarget: p.NamedTarget, Source: p.Source}))
}

func (p actionParams) confirm(ctx context.Context, s *outbound.Service) (any, error) {
	if !p.Yes {
		return actionResult(s.PreviewConfirmation(ctx, p.ID))
	}
	return actionResult(s.Confirm(ctx, p.ID, p.NamedTarget))
}

const needsConfirmationCode = -32003

func actionResult(r outbound.Result, err error) (any, error) {
	if err == nil {
		return r, nil
	}
	switch {
	case errors.Is(err, outbound.NeedsConfirmation), errors.Is(err, outbound.ErrDenied), errors.Is(err, outbound.ErrConflict):
		e := rpcError(protocol.ActionDenied, "外发被拒绝")
		if errors.Is(err, outbound.NeedsConfirmation) {
			e = rpcError(needsConfirmationCode, "外发需要确认")
		}
		if errors.Is(err, outbound.ErrConflict) {
			e.Message = "外发记录状态或幂等键冲突"
		}
		if r.ID != "" {
			raw, err := jsonv2.Marshal(map[string]any{"id": r.ID, "action": r.Action, "level": r.Level, "reversible": r.Reversible, "preview": r.Preview})
			if err != nil {
				return nil, err
			}
			e.Data = new(json.RawMessage(raw))
		}
		return nil, e
	case errors.Is(err, outbound.ErrMissing):
		return nil, rpcError(-32004, "找不到外发记录")
	case errors.Is(err, exthost.ErrUnavailable):
		return nil, rpcError(-32006, "动作所属扩展不可用")
	}
	// 扩展的私有错误码不能冒充本地接口的确认、鉴权或参数错误。
	if _, ok := errors.AsType[*protocol.Error](err); ok {
		return nil, rpcError(-32603, "扩展动作调用失败")
	}
	return nil, err
}
