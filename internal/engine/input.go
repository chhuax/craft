package engine

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
)

// ReadTime 的零值代表旧输入缺少读取记录；Present 且 Time 为空代表明确未读。
type ReadTime struct {
	Present bool
	Time    string
}

// MarshalJSONTo 仅对已送达的输入写 null 或读取时刻，缺字段由外层 omitzero 处理。
func (r ReadTime) MarshalJSONTo(out *jsontext.Encoder) error {
	if !r.Present {
		return fmt.Errorf("缺少 readAt 状态")
	}
	if r.Time == "" {
		return out.WriteToken(jsontext.Null)
	}
	if err := checkReadTime(r.Time); err != nil {
		return err
	}
	return out.WriteToken(jsontext.String(r.Time))
}

// UnmarshalJSONFrom 保留显式 null，避免旧输入被误当成需要重排的新输入。
func (r *ReadTime) UnmarshalJSONFrom(in *jsontext.Decoder) error {
	tok, err := in.ReadToken()
	if err != nil {
		return err
	}
	var value string
	switch tok.Kind() {
	case 'n':
	case '"':
		value = tok.String()
		if err := checkReadTime(value); err != nil {
			return err
		}
	default:
		return fmt.Errorf("readAt 应为 null 或 UTC 毫秒字符串")
	}
	*r = ReadTime{Present: true, Time: value}
	return nil
}

func checkReadTime(value string) error {
	t, err := IsoTime(value)
	if err != nil {
		return fmt.Errorf("readAt: %w", err)
	}
	if t != value {
		return fmt.Errorf("readAt 不是 UTC 毫秒格式 %q", value)
	}
	return nil
}

// WorkInput 由 kind 决定可用字段；text 指针保留合法的空文本。读写边界均核对变体。
type WorkInput struct {
	ID          string        `json:"id"`
	Kind        string        `json:"kind"`
	At          string        `json:"at"`
	TriggerID   string        `json:"triggerId,omitempty"`
	Note        string        `json:"note,omitempty"`
	Human       bool          `json:"human,omitempty,omitzero"`
	Context     bool          `json:"context,omitempty,omitzero"`
	ReadAt      ReadTime      `json:"readAt,omitzero"`
	DeliveredTo string        `json:"deliveredTo,omitempty"`
	DeliveredAt string        `json:"deliveredAt,omitempty"`
	From        string        `json:"from,omitempty"`
	To          string        `json:"to,omitempty"`
	Effect      *PolicyEffect `json:"effect,omitempty"`
	DecisionID  string        `json:"decisionId,omitempty"`
	Text        *string       `json:"text,omitzero"`
}

func (w *WorkInput) validate() error {
	switch w.Kind {
	case "trigger", "resumed", "policy-changed", "reopen", "text", "note":
	default:
		return fmt.Errorf("未知的工作输入 kind %q", w.Kind)
	}
	// 检查字段与变体的对应关系，避免持久化后重排或恢复了错误的对象。
	triggerRef := w.Kind == "trigger" || w.Kind == "resumed"
	policy := w.Kind == "policy-changed"
	text := w.Kind == "text" || w.Kind == "note"
	switch {
	case triggerRef != (w.TriggerID != ""):
		return fmt.Errorf("输入 %q 的 triggerId 与 kind 不符", w.Kind)
	case w.Kind != "trigger" && (w.ReadAt != (ReadTime{}) || w.Note != "" || w.Human || w.Context || w.DeliveredTo != "" || w.DeliveredAt != ""):
		return fmt.Errorf("输入 %q 携带了 trigger 专属字段", w.Kind)
	case policy != (w.From != "") || policy != (w.To != "") || (!policy && w.Effect != nil):
		return fmt.Errorf("输入 %q 的策略变更字段与 kind 不符", w.Kind)
	case (w.Kind == "reopen") != (w.DecisionID != ""):
		return fmt.Errorf("输入 %q 的 decisionId 与 kind 不符", w.Kind)
	case text != (w.Text != nil):
		return fmt.Errorf("输入 %q 的 text 与 kind 不符", w.Kind)
	}
	return nil
}

type inputWire WorkInput

// MarshalJSONTo 在输出前验证变体，直接向聚合使用的编码器写入字段。
func (w WorkInput) MarshalJSONTo(out *jsontext.Encoder) error {
	if err := w.validate(); err != nil {
		return err
	}
	return jsonv2.MarshalEncode(out, (*inputWire)(&w))
}

// UnmarshalJSONFrom 一次读取各字段，成功才替换旧值，缺失的 readAt 因此能恢复为零值。
func (w *WorkInput) UnmarshalJSONFrom(in *jsontext.Decoder) error {
	var wire inputWire
	if err := jsonv2.UnmarshalDecode(in, &wire); err != nil {
		return err
	}
	next := WorkInput(wire)
	if err := next.validate(); err != nil {
		return err
	}
	*w = next
	return nil
}
