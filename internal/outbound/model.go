package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

// Level 仅表示外发后果的确认等级，由扩展声明。
type Level string

const (
	Auto         Level = "auto"
	Confirm      Level = "confirm"
	ConfirmNamed Level = "confirm_named"
	Deny         Level = "deny"
)

var levels = []Level{Auto, Confirm, ConfirmNamed, Deny}

func stricter(base, rule Level) Level {
	if slices.Index(levels, rule) > slices.Index(levels, base) {
		return rule
	}
	return base
}

// Action 对应 manifest.actions 的单项，只有显式登记的只读动作可直接转发。
type Action struct {
	ID         string `json:"id"`
	Level      Level  `json:"level"`
	Reversible bool   `json:"reversible"`
	ReadOnly   bool   `json:"read_only,omitempty"`
}

// Source 只决定是否先登记待办，确认入口不使用它验证身份。
type Source string

const (
	Session Source = "session"
	Worker  Source = "worker"
)

// Request 每次只针对一个动作和一个幂等键；点名只对当前调用有效。
type Request struct {
	Action         string
	Params         json.RawMessage
	IdempotencyKey string
	Source         Source
	Yes            bool
	NamedTarget    string
}

// Reply 保留扩展的原始结果；预览时由适配器提取摘要和单一、完整的目标标识。
type Reply struct {
	Summary string          `json:"summary,omitempty"`
	Target  string          `json:"target,omitempty"`
	Value   json.RawMessage `json:"value,omitzero"`
}

// Verification 区分明确未生效与缺乏证据，只有前者可作为失败结案。
type Verification string

const (
	Applied      Verification = "applied"
	NotApplied   Verification = "not_applied"
	Inconclusive Verification = "inconclusive"
)

// Executor 承接协议调用。预览必须现查事实且没有外部效果。
// Verify 同时返回扩展的原始核实回执；非预览 Execute 的普通错误可能发生在发送之后。
type Executor interface {
	Execute(context.Context, protocol.ActionExecuteParams) (Reply, error)
	Verify(context.Context, protocol.ActionVerifyParams) (Verification, json.RawMessage, error)
}

// State 是持久状态，空值仅用作插入时的前态。
type State string

const (
	Previewing State = "previewing"
	Pending    State = "pending"
	Confirmed  State = "confirmed"
	Executing  State = "executing"
	Done       State = "done"
	Failed     State = "failed"
	Withdrawn  State = "withdrawn"
	Unknown    State = "unknown"
)

type decisionError string

func (e decisionError) Error() string { return string(e) }

// NeedsConfirmation 对应退出码 3，Act 同时返回本次预览。
const NeedsConfirmation = decisionError("needs_confirmation")

var (
	// ErrDenied 对应退出码 4，也可由扩展在重查时报告硬拒绝。
	ErrDenied = errors.New("外发被拒绝")
	// ErrConflict 表示记录前态已改变或操作不适用于当前状态。
	ErrConflict = errors.New("外发记录状态冲突")
	// ErrRateLimited 由仓储在自动发送名额用尽时返回，入口据此转入确认。
	ErrRateLimited = errors.New("自动发送达到窗口上限")
)

// PrepareTransition 供仓储取得写锁后调用。待确认项可在原状态刷新，必须比较修订。
// 只有取得写锁后的时刻能代表窗口准入时间，请求排队期间可能已有其他发送提交。
func (r Result) PrepareTransition(value *Result, limit RateLimit, at time.Time) error {
	if value.ID == "" || value.Revision != r.Revision+1 {
		return fmt.Errorf("外发记录 %q 的修订已变化: %w", value.ID, ErrConflict)
	}
	auto := value.State == Executing && value.Level == Auto
	if auto && !limit.valid() {
		return fmt.Errorf("外发记录 %q 缺少有效频率窗口", value.ID)
	}
	s, next := r.State, value.State
	ok := false
	switch s {
	case "":
		ok = next == Previewing
	case Previewing:
		ok = next == Pending || next == Confirmed || auto || next == Failed
	case Pending:
		ok = next == Confirmed || next == Withdrawn || next == Pending || next == Failed
	case Confirmed:
		ok = next == Executing || next == Pending || next == Failed
	case Executing:
		ok = next == Done || next == Failed || next == Unknown
	}
	if !ok {
		return fmt.Errorf("%s → %s: %w", s, next, ErrConflict)
	}
	value.At = at.UTC()
	if auto {
		value.AutoAt = new(at.UnixMilli())
	} else if next == Failed {
		// 只有确定没有外部效果才释放名额；断连或结果未知仍占用窗口。
		value.AutoAt = nil
	}
	return nil
}

// Result 同时是 outbox 文档。ID 即幂等键；终态和回执随整份文档原子提交。
type Result struct {
	Revision   int64           `json:"revision"`
	ID         string          `json:"id"`
	Action     string          `json:"action"`
	Params     json.RawMessage `json:"params"`
	Source     Source          `json:"source"`
	Level      Level           `json:"level"`
	Reversible bool            `json:"reversible"`
	State      State           `json:"state"`
	Preview    Reply           `json:"preview"`
	Receipt    json.RawMessage `json:"receipt,omitzero"`
	At         time.Time       `json:"at"`
	// AutoAt 记录自动发送占用名额的毫秒时刻，包括回执尚未落盘的调用。
	AutoAt *int64 `json:"autoAt,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// RateLimit 在同一个 outbox 内共享，窗口为左开右闭，精度为毫秒。
type RateLimit struct {
	Max    int
	Window time.Duration
}

func (l RateLimit) valid() bool {
	return l.Max > 0 && l.Window >= time.Millisecond && l.Window%time.Millisecond == 0
}

// Outbox 的 Save 在同一短事务内比对前态、检查转移并保存整份文档。
// 进入自动执行时还须在该事务中读时钟、检查窗口并占位；成功后传回落盘时刻。
// Get 用空 ID 表示缺失，Records 的空状态表示全部，读取结果归调用方所有。
type Outbox interface {
	Get(context.Context, string) (Result, error)
	Records(context.Context, State) ([]Result, error)
	Save(context.Context, *Result, RateLimit, func() time.Time) error
}
