package protocol

import "encoding/json"

// InitializeParams 将主版本与此次启动的配置、状态目录一起交给扩展。
type InitializeParams struct {
	Protocol      int             `json:"protocol"`
	KernelVersion string          `json:"kernelVersion"`
	Config        json.RawMessage `json:"config"`
	StateDir      string          `json:"stateDir"`
}

// InitializeResult 由扩展确认握手；能力声明保留原始 JSON，以便同一主版本内增加字段。
type InitializeResult struct {
	OK           bool            `json:"ok"`
	Capabilities json.RawMessage `json:"capabilities"`
}

// AlarmRunParams 保留原定触发时间，即使实际执行因排队而延后。
type AlarmRunParams struct {
	AlarmID       string `json:"alarmId"`
	Reason        string `json:"reason"`
	ScheduledAtMs int64  `json:"scheduledAtMs"`
}

// ActionExecuteParams 将动作参数原样交给扩展，预览和执行均携带幂等键。
type ActionExecuteParams struct {
	Action         string          `json:"action"`
	Params         json.RawMessage `json:"params"`
	Preview        bool            `json:"preview"`
	IdempotencyKey string          `json:"idempotencyKey"`
}

// ActionVerifyParams 标识需要由扩展核实的那次对外写。
type ActionVerifyParams struct {
	Action         string `json:"action"`
	IdempotencyKey string `json:"idempotencyKey"`
}

// ContextReadParams 只规定寻址字段，触发信息、资源及分页游标的内容由扩展解释。
type ContextReadParams struct {
	Thread    ThreadRef       `json:"thread"`
	Trigger   json.RawMessage `json:"trigger,omitempty"`
	Resource  json.RawMessage `json:"resource,omitempty"`
	Cursor    json.RawMessage `json:"cursor,omitempty"`
	MessageID string          `json:"messageId,omitempty"`
}
