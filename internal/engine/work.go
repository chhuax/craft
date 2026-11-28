package engine

// Work 聚合授权、派工和输入，只持久化事实。
type Work struct {
	ID          string           `json:"id"`
	Title       string           `json:"title"`
	DecisionID  string           `json:"decisionId"`
	Mandate     Mandate          `json:"mandate"`
	Channels    []ChannelRef     `json:"channels"`
	Ended       *WorkEnd         `json:"ended,omitempty"`
	Attention   *Question        `json:"attention,omitempty"`
	Assignments []Assignment     `json:"assignments"`
	Inputs      []WorkInput      `json:"inputs"`
	Undelivered []string         `json:"undelivered"`
	Follows     []Follow         `json:"follows,omitempty"`
	Origin      Origin           `json:"origin"`
	Execution   *Execution       `json:"execution,omitempty"`
	Delivery    *DeliveryFailure `json:"delivery,omitempty"`
	CreatedAt   string           `json:"createdAt"`
	UpdatedAt   string           `json:"updatedAt"`
}

// WorkEnd 标记工作结束，保留结束方以区分人的停止决定。
type WorkEnd struct {
	At      string      `json:"at"`
	By      string      `json:"by"`
	Outcome WorkOutcome `json:"outcome"`
	Reason  string      `json:"reason,omitempty"`
}

// Question 保留问题身份和答复时刻，使同文的新问题仍可区分。
type Question struct {
	ID         string `json:"id"`
	Question   string `json:"question"`
	TriggerID  string `json:"triggerId,omitempty"`
	CreatedAt  string `json:"createdAt,omitempty"`
	AnsweredAt string `json:"answeredAt,omitempty"`
}

// Origin 保留起源事件及与它一起被接下的整批事件。
type Origin struct {
	TriggerID  string   `json:"triggerId"`
	TriggerIDs []string `json:"triggerIds"`
}

// Execution 保存同一工作在各次派工间复用的执行位置和会话。
type Execution struct {
	Target       *TargetSnapshot `json:"target,omitempty"`
	Directory    string          `json:"directory,omitempty"`
	Session      *Session        `json:"session,omitempty"`
	Bootstrapped *bool           `json:"bootstrapped,omitempty"`
	Assignment   string          `json:"assignment,omitempty"`
}

// TargetSnapshot 保留选定目标时的标识、路径和证据。
type TargetSnapshot struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Path       string   `json:"path"`
	Title      string   `json:"title"`
	Evidence   []string `json:"evidence"`
	SelectedBy string   `json:"selectedBy,omitempty"`
}

// Session 标识宿主上的执行会话。
type Session struct {
	Host string `json:"host"`
	ID   string `json:"id"`
}

// DeliveryFailure 保留交付故障及其发生时间。
type DeliveryFailure struct {
	Error string `json:"error"`
	At    string `json:"at"`
}

// Assignment 记录一次派工与这次派工的最近进展。
type Assignment struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	CreatedAt   string  `json:"createdAt"`
	StartedAt   string  `json:"startedAt,omitempty"`
	EndedAt     string  `json:"endedAt,omitempty"`
	EndedReason string  `json:"endedReason,omitempty"`
	Report      *Report `json:"report,omitempty"`
	DeliveredAt string  `json:"deliveredAt,omitempty"`
	IdleAt      string  `json:"idleAt,omitempty"`
}

// Report 保留执行方在本次派工中报告的状态与摘要。
type Report struct {
	State   ReportState `json:"state"`
	Summary string      `json:"summary"`
	At      string      `json:"at"`
}

// Follow 记录工作对对象的跟随关系和建立依据。
type Follow struct {
	Thread string        `json:"thread"`
	Own    bool          `json:"own"`
	Closed *ThreadClose  `json:"closed,omitempty"`
	Via    []string      `json:"via,omitempty"`
	Manual *ManualFollow `json:"manual,omitempty"`
}

// ManualFollow 保留手工指定的归属，以便自动来源撤回后恢复。
type ManualFollow struct {
	Own bool `json:"own"`
}

// PolicyEffect 用于改版输入，显式 false 与未受影响须可区分。
type PolicyEffect struct {
	Rules            []RuleChange `json:"rules,omitempty"`
	RequesterAllowed *bool        `json:"requesterAllowed,omitempty"`
	Requester        *ActorRef    `json:"requester,omitempty"`
	Proactive        *bool        `json:"proactive,omitempty"`
}

// RuleChange 保留受影响规则的显示名及变更后的执行说明。
type RuleChange struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	Change      string  `json:"change"`
	Instruction *string `json:"instruction,omitzero"`
}

// WorkOutcome 表示工作的最终结果。
type WorkOutcome string

const (
	Completed WorkOutcome = "completed"
	Done      WorkOutcome = "done"
	Closed    WorkOutcome = "closed"
	Dropped   WorkOutcome = "dropped"
	Stopped   WorkOutcome = "stopped"
)

// ReportState 表示执行方报告的进度，与工作的最终结果分别保存。
type ReportState string

const (
	Progress      ReportState = "progress"
	Waiting       ReportState = "waiting"
	Ready         ReportState = "ready"
	Blocked       ReportState = "blocked"
	Failed        ReportState = "failed"
	ReportDropped ReportState = "dropped"
)
