// Package host 定义执行方与宿主会话之间的边界。驱动只返回宿主事实，派工代次由调用方核对。
package host

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// Driver 管理持久会话。未知投递必须由人按标记核对，驱动和调用方均不可自动重发。
type Driver interface {
	Capabilities() Capabilities
	Create(context.Context, CreateSpec) (Session, error)
	Append(context.Context, Session, string, string) Result
	Stop(context.Context, Session, time.Duration) error
	Status(context.Context, Session) State
	LastReply(context.Context, Session) (string, error)
	Open(context.Context, Session) error
}

// Capabilities 描述宿主经过验证的能力；受持有写入权等条件限制时，逐次操作仍需判断。
type Capabilities struct {
	CreateSeparatesPrompt bool
	BusyAppend            bool
	StopHook              bool
	SessionStatus         bool
	LastReply             bool
	WorkspaceBoundary     bool
}

// Session 只携带宿主的规范 ID，短显示号不得用作执行方身份。
type Session struct{ Host, ID string }

// CreateSpec 的 Env 由派工方构造。OnCreated 在获得规范 ID 时同步登记；拒绝登记时须保留 ID 供停止。
type CreateSpec struct {
	Directory, Title, Prompt, DeliveryID string
	Env                                  map[string]string
	Permissions                          Permissions
	OnCreated                            func(Session) error
}

// Permissions 指定必须落实的写范围、工具集合与断网要求。Tools 为 nil 时沿用宿主工具集。
type Permissions struct {
	WriteScope WriteScope
	Tools      []string
	NoNetwork  bool
}

// WriteScope 描述派工所需的文件边界，零值采用宿主默认权限。
type WriteScope string

// 写范围按实际能力编译；不能保证时返回 Unsupported。
const (
	ReadOnly  WriteScope = "readonly"
	Workspace WriteScope = "workspace"
	Worktree  WriteScope = "worktree"
)

// State 是会话当前的可观察状态。查询失败、记录含糊时返回 UnknownState。
type State string

// 等待审批的会话不能视为空闲；只有明确消失的记录才使用 Gone。
const (
	Busy         State = "busy"
	Idle         State = "idle"
	Gone         State = "gone"
	UnknownState State = "unknown"
)

// Outcome 也可包装在 Create、Stop 等操作的 error 中，用 errors.Is 判断。
type Outcome string

// Later 表示本次尚未发送；Unknown 表示可能已经生效，必须保留投递标记核对。
const (
	OK          Outcome = "OK"
	Later       Outcome = "Later"
	Unknown     Outcome = "UNKNOWN"
	Unsupported Outcome = "UNSUPPORTED"
)

// Error 让带核对说明的错误仍可按宿主结果分类。
func (o Outcome) Error() string { return string(o) }

// Result 将投递结果与核对办法一起交给调用方；零值不代表成功。
type Result struct {
	Outcome Outcome
	Err     error
}

var deliveryID = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// MarkDelivery 验证并前置投递标记，使正文被宿主截断后仍可人工对账。
func MarkDelivery(id, text string) (string, error) {
	if !deliveryID.MatchString(id) {
		return "", fmt.Errorf("%w: 投递 ID 无效", Unsupported)
	}
	return "[craft:delivery:" + id + "]\n" + text, nil
}
