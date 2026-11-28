package protocol

import (
	"encoding/json"
	"fmt"
)

const JSONRPCVersion = "2.0"

// Request 保留调用方选择的标识类型，避免较大的整数经浮点数转换后失真。
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      json.RawMessage  `json:"id"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
}

// Notification 独立于 Request，发送方可以据此表达无需回应的调用。
type Notification struct {
	JSONRPC string           `json:"jsonrpc"`
	Method  string           `json:"method"`
	Params  *json.RawMessage `json:"params,omitempty"`
}

// Response 成功时只设 Result（包括显式的 null），失败时只设 Error。
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error 的 Data 留给方法定义，使扩展能为协议错误附带结构化说明。
type Error struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Data    *json.RawMessage `json:"data,omitempty"`
}

// Error 让调用方可用 errors.As 取回远端错误码和原始详情。
func (e *Error) Error() string { return fmt.Sprintf("RPC %d: %s", e.Code, e.Message) }
