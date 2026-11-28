package engine

import "context"

// Executor 接受运行层派发的会话操作。调用方在每次操作前核对代次，并将结果作为引擎命令回写。
type Executor interface {
	Start(context.Context, Work, Assignment) (ExecutorResult, error)
	Deliver(context.Context, Work, Assignment) (ExecutorResult, error)
	Stop(context.Context, Work, Assignment, string) (ExecutorResult, error)
}

// ExecutorResult 区分确定送达、尚不能发送及宿主无法确认结果。核对办法由适配器随错误说明提供。
type ExecutorResult string

const (
	ExecutorOK      ExecutorResult = "OK"
	ExecutorLater   ExecutorResult = "Later"
	ExecutorUnknown ExecutorResult = "UNKNOWN"
)

// Preparer 在运行层规划的目标上准备派工目录。
type Preparer interface {
	Prepare(context.Context, TargetSnapshot, Work) (PreparedTarget, error)
}

// PreparedTarget 指定可供执行方使用的实际目录。
type PreparedTarget struct{ Directory string }

// Channel 由运行层消费 reply effect 时调用，消息 ID 供 outbound 幂等外发。
type Channel interface {
	Send(context.Context, OutboundMessage) (ReplyStatus, error)
}

// OutboundMessage 包含唯一消息标记和确定的回复地址，供运行层在事务提交后投递。
type OutboundMessage struct {
	ID      string      `json:"id"`
	Kind    string      `json:"kind"`
	WorkID  string      `json:"workId,omitempty"`
	Channel *ChannelRef `json:"channel"`
	Text    string      `json:"text"`
}

// ReplyStatus 记录外发消息的状态，confirm 等待人的确认。
type ReplyStatus string

const (
	ReplyPending ReplyStatus = "pending"
	ReplySent    ReplyStatus = "sent"
	ReplyDropped ReplyStatus = "dropped"
	ReplyConfirm ReplyStatus = "confirm"
)
