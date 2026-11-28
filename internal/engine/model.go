package engine

import "encoding/json"

// Trigger 保存扩展提交的不可变事件快照。
type Trigger struct {
	ID        string                     `json:"id"`
	Source    string                     `json:"source"`
	Event     string                     `json:"event"`
	Threads   []ThreadRef                `json:"threads"`
	Actor     ActorRef                   `json:"actor"`
	Human     bool                       `json:"human"`
	Addressed string                     `json:"addressed,omitempty"`
	At        string                     `json:"at"`
	Text      string                     `json:"text"`
	Channel   *ChannelRef                `json:"channel,omitempty"`
	URL       string                     `json:"url,omitempty"`
	Meta      map[string]json.RawMessage `json:"meta"`
}

// TriggerDisposition 记录事件去向，供分发、重排及交回时核对。
type TriggerDisposition struct {
	Filtered   *Reason    `json:"filtered,omitempty"`
	Recorded   bool       `json:"recorded,omitempty,omitzero"`
	EvalID     string     `json:"evalId,omitempty"`
	DecisionID string     `json:"decisionId,omitempty"`
	Fanout     []string   `json:"fanout"`
	Context    []string   `json:"context,omitempty"`
	Requeued   bool       `json:"requeued,omitempty,omitzero"`
	Handbacks  []Handback `json:"handbacks,omitempty"`
	At         string     `json:"at"`
}

// Handback 记下哪件工作在何时退回了事件。
type Handback struct {
	WorkID string `json:"workId"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// Thread 按外部对象聚合事件，至多有一批进行中的研判。
type Thread struct {
	Key           string       `json:"key"`
	Ref           ThreadRef    `json:"ref"`
	CreatedAt     string       `json:"createdAt"`
	LastAt        string       `json:"lastAt,omitempty"`
	Subscriptions []string     `json:"subscriptions"`
	Context       []string     `json:"context,omitempty"`
	Correlated    []string     `json:"correlated"`
	Pending       []string     `json:"pending"`
	Evaluating    *Evaluation  `json:"evaluating,omitempty"`
	Closed        *ThreadClose `json:"closed,omitempty"`
	ReopenedAt    string       `json:"reopenedAt,omitempty"`
	Meta          ThreadMeta   `json:"meta"`
}

// ThreadClose 保留对象关闭时的事实，供对象及其跟随记录共用。
type ThreadClose struct {
	At      string `json:"at"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
}

// ThreadMeta 保存扩展描述的最近一次对象信息。
type ThreadMeta struct {
	Label string          `json:"label,omitempty"`
	URL   string          `json:"url,omitempty"`
	Facts map[string]Fact `json:"facts"`
}

// Fact 保存扩展事实值；保留原始数值及嵌套结构，避免解读未知事实。
type Fact struct {
	Value   json.RawMessage  `json:"value"`
	Label   string           `json:"label,omitempty"`
	Problem *json.RawMessage `json:"problem,omitempty"`
}

// Evaluation 固定一次研判的事件批次和所用策略。
type Evaluation struct {
	ID            string       `json:"id"`
	ThreadKey     string       `json:"threadKey"`
	TriggerIDs    []string     `json:"triggerIds"`
	Primary       string       `json:"primary,omitempty"`
	Strength      Strength     `json:"strength"`
	Actor         ActorRef     `json:"actor"`
	Basis         MandateBasis `json:"basis"`
	PolicyVersion string       `json:"policyVersion"`
	StartedAt     string       `json:"startedAt"`
}

// Decision 保存研判结论与产生结论的依据。
type Decision struct {
	ID            string                     `json:"id"`
	EvalID        string                     `json:"evalId"`
	ThreadKey     string                     `json:"threadKey"`
	TriggerIDs    []string                   `json:"triggerIds"`
	PolicyVersion string                     `json:"policyVersion"`
	At            string                     `json:"at"`
	Kind          DecisionKind               `json:"kind"`
	Reason        []Reason                   `json:"reason"`
	Strength      Strength                   `json:"strength"`
	WorkID        string                     `json:"workId,omitempty"`
	Mandate       *Mandate                   `json:"mandate,omitempty"`
	Audit         map[string]json.RawMessage `json:"audit,omitempty"`
	Dismissed     bool                       `json:"dismissed,omitempty,omitzero"`
}

// Mandate 固定接活时的授权快照，调用方须保留原始范围和依据。
type Mandate struct {
	Strength      Strength     `json:"strength"`
	PolicyVersion string       `json:"policyVersion"`
	Basis         MandateBasis `json:"basis"`
	Actor         ActorRef     `json:"actor"`
	Scope         string       `json:"scope"`
}

// MandateBasis 记录准入路径与当时依赖的事件、发送人和规则。
type MandateBasis struct {
	Via    Via      `json:"via"`
	Source string   `json:"source"`
	Event  string   `json:"event"`
	Sender string   `json:"sender,omitempty"`
	Mode   Mode     `json:"mode,omitempty"`
	Rules  []string `json:"rules,omitempty"`
}

// Effect 保存副作用队列行的形状，序号与代次由写入方分配。
type Effect struct {
	Kind       string          `json:"kind"`
	Key        string          `json:"key"`
	Value      json.RawMessage `json:"value"`
	Seq        int64           `json:"seq"`
	Generation int64           `json:"generation"`
	Attempts   int             `json:"attempts"`
	Error      string          `json:"error"`
	Due        string          `json:"due"`
	Since      string          `json:"since"`
}

// 四个约定键可跨扩展解释，其余键只由来源解释。
const (
	FactTitle   = "title"
	FactState   = "state"
	FactDue     = "due"
	FactUrgency = "urgency"
)

// Strength 区分明确授权与推断候选。
type Strength string

const (
	Explicit Strength = "explicit"
	Inferred Strength = "inferred"
)

// DecisionKind 区分同一事件批次可能产生的研判结论。
type DecisionKind string

const (
	Ignore      DecisionKind = "ignore"
	Accept      DecisionKind = "accept"
	Reopen      DecisionKind = "reopen"
	Stop        DecisionKind = "stop"
	Unavailable DecisionKind = "unavailable"
)

// Mode 是人对一类事件设定的处理方式；取值与 02 §4.1 的 settings 一致。
type Mode string

// 事件的处理方式：off 完全不接，observe 只记录「本会接」，auto 直接接活。
const (
	ModeOff     Mode = "off"
	ModeObserve Mode = "observe"
	ModeAuto    Mode = "auto"
)

// Via 记录授权是怎样产生的，用于事后审计与恢复前的重新校验。
type Via string

// 授权来源，见 01 §5 的 MandateBasis。
const (
	ViaAddressed     Via = "addressed"
	ViaFollowup      Via = "followup"
	ViaEvent         Via = "event"
	ViaPersonRequest Via = "personRequest"
	ViaHumanDirect   Via = "humanDirect"
)

// Admission 是 Policy.Admits 的结论：ok 建单、observe 只记录、refuse 拒绝。
type Admission string

// Admits 的三种结论。
const (
	AdmitOK      Admission = "ok"
	AdmitObserve Admission = "observe"
	AdmitRefuse  Admission = "refuse"
)
