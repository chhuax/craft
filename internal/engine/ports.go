package engine

import (
	"context"
	"encoding/json"
)

// Clock 由调用方注入，返回 ISO 8601 UTC 毫秒字符串。
type Clock func() string

// ScopeResult 给出是否监听该事件；拒绝原因供处置记录使用。
type ScopeResult struct {
	Listened bool
	Reason   string
}

// SourceScope 只投影来源的监听与关闭能力，实现在命令事务内不得执行 IO。
type SourceScope interface {
	Scope(source, event string, meta map[string]json.RawMessage) ScopeResult
	Closable(source, kind string) bool
}

// Event 是供通知与补拉的外发事件。Rev 由 Emit 在事务内分配。
type Event struct {
	Rev  int64                      `json:"rev"`
	Type string                     `json:"type"`
	At   string                     `json:"at"`
	Ref  map[string]string          `json:"ref"`
	Data map[string]json.RawMessage `json:"data"`
}

// Store 串行提交写事务。Read 使用一致的已提交快照，读回调不持有写锁。
// 订阅者在最外层提交后、锁外同步收到独占的事件副本。不同事务的回调可并发，
// 订阅者共享状态须自行同步，跨事务的提交顺序以事件记录的 rev 为准。
type Store interface {
	Atomic(ctx context.Context, fn func(Unit) error) error
	Read(ctx context.Context, fn func(Reader) error) error
	Subscribe(func(Event))
}

// Acceptance 只定位首次建单，不为批内每个事件重复加载整份历史决策。
type Acceptance struct {
	DecisionID string
	WorkID     string
}

// DeliveryWindow 是同一 Work 的投递视图。Head 为队首，Inputs 只含按键命中的输入。
// 整个视图在写事务内读取和回写，后来到达的队尾与其他历史字段由仓储保留。
type DeliveryWindow struct {
	Ended       bool
	Assignment  *Assignment
	Failure     *DeliveryFailure
	Batch, Head []string
	Inputs      map[string]WorkInput
	UpdatedAt   string
}

// Reader 仅供当前事务回调使用。所有查询结果（包括嵌套字段）均由调用方独占。
// 单键缺失返回 nil, nil；批量按键取，省略缺失项，重复键只返回一份。
type Reader interface {
	// GetReplyStatus 返回跨队列消费保留的消息状态，空串表示从未入队。
	GetReplyStatus(messageID string) (ReplyStatus, error)
	// HasEffect 按队列主键检查尚未完成的请求，供中断恢复判定。
	HasEffect(kind, key string) (bool, error)
	GetTrigger(id string) (*Trigger, error)
	GetTriggers(ids ...string) (map[string]*Trigger, error)
	GetDisposition(triggerID string) (*TriggerDisposition, error)
	GetDispositions(triggerIDs ...string) (map[string]*TriggerDisposition, error)
	GetThread(key string) (*Thread, error)
	GetThreads(keys ...string) (map[string]*Thread, error)
	// GetDeliveryWindow 仅加载队首至多 len(ids) 项及按 ID 命中的 inputs。
	GetDeliveryWindow(workID string, ids []string) (*DeliveryWindow, error)
	GetWork(id string) (*Work, error)
	GetWorks(ids ...string) (map[string]*Work, error)
	GetDecision(id string) (*Decision, error)
	GetDecisions(ids ...string) (map[string]*Decision, error)
	// 关联从任一端按键查询，同一对标识只出现一次。
	ThreadKeys(triggerID string) ([]string, error)
	TriggerIDs(threadKey string) ([]string, error)
	// 未关闭、无 evaluating 且有 pending，按首次插入顺序返回。
	ActiveThreadsWithPending() ([]Thread, error)
	// 在办 Work 按首次插入的倒序返回；改写和恢复不改变插入位置。
	OpenWorks() ([]Work, error)
	// RecentOpenWorks 按 createdAt 倒序（同毫秒按首次插入倒序）返回至多 limit 个未排除的在办 Work，非正上限返回空。
	RecentOpenWorks(limit int, exclude ...string) ([]Work, error)
	// OpenOriginWorkID 返回以该对象为主起源的最近插入的在办工作，退订不改变起源。缺失时返回空串。
	OpenOriginWorkID(threadKey string) (string, error)
	// SubscribedThreadKeys 从 Work 反查全部普通和只读订阅，包含未登记为 Follow 的对象。
	SubscribedThreadKeys(workID string) ([]string, error)
	// EvaluatingThreads 用于改版时取得所有仍持有研判批次的对象。
	EvaluatingThreads() ([]Thread, error)
	// ThreadByEval 仅命中当前仍在进行的研判，结束或被替换后立即失效。
	ThreadByEval(evalID string) (*Thread, error)
	// AcceptedByTrigger 按批内事件键查曾经建单的决策，后续 ignore 不抹去授权历史。
	AcceptedByTrigger(ids ...string) (map[string]Acceptance, error)
	// DecisionsByThread 返回该对象的全部决策，顺序未指定。
	DecisionsByThread(key string) ([]Decision, error)
	// EventsSince 按 rev 升序补拉严格晚于游标的至多 limit 条记录。
	EventsSince(rev int64, limit int) ([]Event, error)
}

// Effects 把请求随状态变更入队；生产适配器负责按 (kind, key) 合并。
type Effects interface {
	Enqueue(kind, key string, value json.RawMessage) error
}

// Unit 仅供当前回调在同一 goroutine 使用。Put 后调用方可继续修改原对象。
// 嵌套写使用 Unit.Atomic；内层回调只用自己的 Unit。内层失败仅撤销本层，
// 内层成功仍受外层回滚约束。AfterCommit 按注册顺序在最外层提交后运行；
// 回滚丢弃回调，单个回调 panic 只记日志，不影响提交结果及后续回调。
type Unit interface {
	Reader
	Atomic(ctx context.Context, fn func(Unit) error) error
	AfterCommit(fn func())
	// Trigger 是不可变事实；重复写入同一 ID 保留第一份记录。
	PutTrigger(Trigger) error
	PutDisposition(triggerID string, value TriggerDisposition) error
	PutThread(Thread) error
	PutWork(Work) error
	// PutDeliveryWindow 仅回写该事务所读批次的投递字段，并从已核对的队首移除 consumed 项。
	PutDeliveryWindow(workID string, window DeliveryWindow, consumed int) error
	PutDecision(Decision) error
	// PutReplyStatus 与回复入队或外发回执同事务写入。
	PutReplyStatus(messageID string, status ReplyStatus) error
	// LinkTrigger 重复登记同一对键不增加关联。
	LinkTrigger(triggerID, threadKey string) error
	Effects() Effects
	// Emit 同事务递增 rev 并追加事件，只在提交后广播。
	Emit(Event) error
}
