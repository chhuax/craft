package app

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

type replyHandler struct {
	ready    atomic.Bool
	repo     engine.Store
	commands *engine.Engine
	box      *outbound.Service
	actions  map[string]string
	health   *daemonHealth
}

// Kind 对应引擎提交的两类渠道消息。
func (*replyHandler) Kind() string { return "reply" }

// Limit 在渠道协议没有并行度声明时一次只外发一条。
func (h *replyHandler) Limit() int {
	if h.ready.Load() {
		return 1
	}
	return 0
}

// Ordered 允许其他消息在单条预览退避时继续。
func (*replyHandler) Ordered() bool { return false }

// Quiet 让持续失败的预览或存储进入健康告警。
func (*replyHandler) Quiet() bool { return false }

// Run 先检查消息是否仍待外发，处理器写回后由调度器按代次消费队列。
func (h *replyHandler) Run(ctx context.Context, row store.Effect) EffectResult {
	var m engine.OutboundMessage
	if err := jsonv2.Unmarshal(row.Value, &m); err != nil {
		return EffectResult{Err: fmt.Errorf("解码回复 %q: %w", row.Key, err)}
	}
	if m.ID == "" || m.ID != row.Key {
		return EffectResult{Err: fmt.Errorf("回复 %q 的消息标识不匹配", row.Key)}
	}
	var before engine.ReplyStatus
	err := h.repo.Read(ctx, func(r engine.Reader) error {
		var err error
		before, err = r.GetReplyStatus(m.ID)
		return err
	})
	if err != nil {
		return EffectResult{Err: err}
	}
	switch before {
	case engine.ReplySent, engine.ReplyConfirm, engine.ReplyFailed, engine.ReplyUnknown:
		return EffectResult{}
	case engine.ReplyDropped:
		r, err := h.box.Show(ctx, m.ID)
		if errors.Is(err, outbound.ErrMissing) {
			return EffectResult{}
		}
		if err == nil && r.State == outbound.Pending {
			err = h.box.Withdraw(ctx, m.ID)
		}
		return EffectResult{Err: err}
	case engine.ReplyPending:
		return EffectResult{Err: h.deliver(ctx, m)}
	default:
		return EffectResult{Err: fmt.Errorf("回复 %q 的入队状态无效 %q", m.ID, before)}
	}
}

func (h *replyHandler) deliver(ctx context.Context, m engine.OutboundMessage) error {
	status, cause := h.Send(ctx, m)
	if status == "" {
		return cause
	}
	var detail string
	if cause != nil {
		detail = cause.Error()
	}
	changed, err := h.commands.FinishReply(ctx, m, status, detail)
	if err != nil {
		return fmt.Errorf("记录回复 %q 的结果: %w", m.ID, err)
	}
	if changed && status == engine.ReplyFailed {
		h.health.report("reply", m.ID, HealthEntry{"error", detail})
	}
	return nil
}

// Send 将渠道视为不透明地址，仅 outbound 有权决定是否实际发送。
func (h *replyHandler) Send(ctx context.Context, m engine.OutboundMessage) (engine.ReplyStatus, error) {
	if m.Channel == nil || len(m.Channel.Thread) == 0 || m.Kind != engine.ReplyReply && m.Kind != engine.ReplyAcknowledgement {
		return engine.ReplyFailed, fmt.Errorf("回复 %q 的类型或渠道地址无效", m.ID)
	}
	action := h.actions[m.Channel.Source]
	if action == "" {
		return engine.ReplyFailed, fmt.Errorf("渠道 %q 没有已声明的回复动作", m.Channel.Source)
	}
	params, err := jsonv2.Marshal(struct {
		Thread json.RawMessage `json:"thread"`
		Body   string          `json:"body"`
	}{m.Channel.Thread, m.Text})
	if err != nil {
		return engine.ReplyFailed, fmt.Errorf("编码回复 %q 的参数: %w", m.ID, err)
	}
	req := outbound.Request{Action: action, Params: params, IdempotencyKey: m.ID, Source: outbound.Worker}
	// effect 的确认回写可能晚于 outbox 提交。已有回执时连预览也无需重做。
	previous, err := h.box.Show(ctx, m.ID)
	if err == nil {
		if status, cause := replyReceipt(previous, req, nil); status != "" {
			return status, cause
		}
	} else if !errors.Is(err, outbound.ErrMissing) {
		return "", err
	}
	r, err := h.box.Act(ctx, req)
	if r.ID != "" {
		if status, cause := replyReceipt(r, req, err); status != "" {
			return status, cause
		}
	}
	if err == nil {
		return "", fmt.Errorf("回复 %q 缺少外发回执", m.ID)
	}
	if rpc, ok := errors.AsType[*protocol.Error](err); (ok && rpc.Code == -32602) || errors.Is(err, outbound.ErrDenied) {
		return engine.ReplyFailed, err
	}
	return "", err
}

func replyReceipt(r outbound.Result, req outbound.Request, cause error) (engine.ReplyStatus, error) {
	if !r.MatchesRequest(req) {
		return engine.ReplyFailed, fmt.Errorf("回复 %q 的幂等键已绑定其他外发请求: %w", req.IdempotencyKey, outbound.ErrConflict)
	}
	switch r.State {
	case outbound.Done:
		return engine.ReplySent, nil
	case outbound.Pending:
		return engine.ReplyConfirm, nil
	case outbound.Executing, outbound.Unknown:
		// 扩展可能已生效，保留执行标记供启动恢复核实。
		return engine.ReplyUnknown, cause
	case outbound.Failed:
		if cause == nil {
			cause = fmt.Errorf("外发记录 %q 已失败：%s", r.ID, r.Detail)
		}
		return engine.ReplyFailed, cause
	case outbound.Withdrawn:
		return engine.ReplyDropped, nil
	}
	return "", cause
}
