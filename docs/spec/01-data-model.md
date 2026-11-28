# 01 数据模型

状态：v1 草案。以 JSON 描述，字段名 camelCase；Go 结构体用 tag 对应。

## 1. 约定

- **时间**：ISO 8601 UTC 毫秒字符串 `YYYY-MM-DDTHH:MM:SS.sssZ`，字符串比较即时间比较。引擎只用 `addMs` / `diffMs` 做时间运算；`now` 由外部注入，引擎不读系统时钟。
- **确定性 id**：`key(values...) = sha256(json(values))[:32]`。同样输入永远得到同样 id，所以重复执行命令是幂等的。
- **JSON 边界**：持久化与跨进程一律严格 JSON，禁 `NaN/Infinity`。可选字段为 null 时不写出。唯一三态例外见 `WorkInput.readAt`。
- **聚合整体读写**：每条命令开头在事务内按 key 读聚合，结尾写回；禁止跨命令缓存。多进程写同一个库，事务外读到的对象只用来挑选要处理哪些。

## 2. 引用

```
ThreadRef  { source, kind, id }          // 扩展声明的对象；source 是扩展 manifest 里 watch_sources 声明的来源 id，每个 source 只归一个扩展
ActorRef   { source, id, name? }
ChannelRef { source, thread, ... }       // 回复送去哪；内核只比较整体相等
threadKey(ref) = "<source>:<kind>:<id>"
```

## 3. Trigger（事件）

永不修改。

```
Trigger {
  id        string         // 扩展生成，确定性；同一外部事件永远同一 id
  source    string         // 扩展 id
  event     string         // 扩展声明的事件类型，如 "item.assigned"
  threads   ThreadRef[]    // 至少一个；threads[0] 为主 Thread
  actor     ActorRef
  human     bool           // 扩展声明：人做的（不是机器人/CI/本系统自己）
  addressed "direct" | "mention" | null   // 是否冲着我
  at        time
  text      string         // 上限 4000 字符，超出由内核截断并置 meta.truncated=true
  channel   ChannelRef?
  url       string?
  meta      object         // 扩展原生字段，内核不解读
}
```

事件详情随事件一起入队，不另写 KV。对象「现在是什么样」由扩展经 `thread/describe` 写在 Thread 的 facts 上；完整原文、附件、长历史不进内核，执行方需要时经扩展的读接口现查。

**内置人事件**（`source = "craft"`，不可配置，不由扩展声明）：

| 事件 | 含义 | 属性 |
|---|---|---|
| `craft:person.answer` | 人回答 Work 的问题；**只投给被回答的那个 Work**，不走订阅分发 | `explicit`、`auto`、`followup_only` |
| `craft:person.request` | 人把某对象交给系统处理，或采纳盘点建议 | `explicit`、`auto` |
| `craft:person.direct` | 人直接开的会话（`mandate.basis.via = humanDirect`） | `explicit`、`auto` |

**TriggerDisposition**：每个 Trigger 一条处置记录，`{filtered?, recorded?, evalId?, decisionId?, fanout[], context[]?, requeued?, handbacks[]?, at}`。

## 4. Thread（被盯的对象）

```
Thread {
  key, ref, createdAt, lastAt?
  subscriptions string[]       // 订阅它的 Work id
  context?      string[]       // 只读跟随的 Work id
  correlated    string[]       // 与它同事件出现过的其他 Thread key
  pending       string[]       // 待研判的 Trigger id（攒批缓冲）
  evaluating?   Evaluation     // 进行中的研判，至多一个
  closed?       { at, outcome: "done"|"closed", detail? }
  reopenedAt?   time
  meta { label?, url?, facts }
}
```

- 「最后一次状态事实的时间」= `max(closed.at, reopenedAt)`。迟到的、更早的关闭/重开一律忽略（旧轮询结果不得覆盖新状态）。
- **facts**：`{factId: {value, label?, problem?}}`。内核只认四个**约定键**用于排序与展示：`title`、`state`、`due`（ISO 日期或时间）、`urgency`（`low|normal|high|urgent`）。取值由扩展翻译好；内核存而不解读其他键。

## 5. Evaluation / Decision / Mandate

```
Strength = "explicit" | "inferred"
Evaluation { id, threadKey, triggerIds[], primary?, strength, actor, basis, policyVersion, startedAt }
DecisionKind = "ignore" | "accept" | "reopen" | "stop" | "unavailable"
Decision { id, evalId, threadKey, triggerIds[], policyVersion, at, kind,
           reason[], strength, workId?, mandate?, audit?, dismissed? }
Mandate  { strength, policyVersion, basis, actor, scope }       // 不可变
MandateBasis { via: "addressed"|"followup"|"event"|"personRequest"|"humanDirect",
               source, event, sender?, mode?: "observe"|"auto", rules?: string[] }
```

- **Mandate 是不可变授权**：谁要做、因何被准入、范围、在哪版策略下。执行以它为界，之后不变。
- `Decision.audit` 是研判方给的依据（满足「推测必须带依据」）。

## 6. Work 及其部件

```
Work {
  id, title, decisionId, mandate, channels[],
  ended?       { at, by: "executor"|"human", outcome, reason? }
  attention?   { id, question, triggerId?, createdAt?, answeredAt? }
  assignments  Assignment[]
  inputs       WorkInput[]
  undelivered  string[]                 // 还没交给执行方的 input id
  follows?     Follow[]
  origin       { triggerId, triggerIds[] }
  execution?   { target?: TargetSnapshot, directory?, session?: {host, id}, bootstrapped?: bool, assignment? }   // 执行方适配层填；TargetSnapshot = {id, kind, path, title, evidence, selectedBy?}
  delivery?    { error, at }
  createdAt, updatedAt
}
WorkOutcome = "completed" | "done" | "closed" | "dropped" | "stopped"

Assignment { id, state: "pending"|"running"|"ended", createdAt, startedAt?, endedAt?,
             endedReason?, report?: {state, summary, at}, deliveredAt?, idleAt? }
ReportState = "progress"|"waiting"|"ready"|"blocked"|"failed"|"dropped"

Follow { thread, own: bool, closed?: {at, outcome, detail?}, via?: string[], manual?: {own} }
```

**WorkInput**（按 `kind` 区分）：

| kind | 字段 |
|---|---|
| `trigger` | `id, triggerId, at, note?, human?, context?, readAt?, deliveredTo?, deliveredAt?` |
| `policy-changed` | `id, from, to, at, effect?` |
| `reopen` | `id, decisionId, at` |
| `resumed` | `id, triggerId, at` |
| `text` / `note` | `id, text, at` |

`trigger.readAt` 是三态：字段不存在（旧版本送达，未记录读取）、`null`（已送达、会话还没取走；重启只重排这一类）、时间字符串（已读）。Go 用 `*Nullable[time]` 或自定义类型表示，仓储必须有往返测试。

**Follow 与结束规则**：

- `own=true` 的 Follow 是 Work 自己的对象。执行方报 `ready` 后，等所有 own Thread 关闭 Work 才结束；own Thread 关闭时工作已完成则直接结束。无 follows 时 `ready` 即 `completed`。
- **单自有对象互斥**：任一可关闭对象同时至多归属一个 open Work 的 `own=true` Follow。所有登记自有对象的入口（accept、attach、bindThread、人发起）在同一事务内校验，冲突拒绝 `threadAlreadyOwned`。
- `via` 记录该 own Follow 由哪些 Trigger 自动建立；`manual` 记手工绑定时的 own 值，自动来源撤回后恢复。

## 7. 阶段（只算不存）

`workStage(work)` 是纯函数，每次现算，不落库：

```
person(delivery|answer|review|blocked|failed) | working(queued) | idle | ended
```

按顺序第一条命中。`attention.answeredAt` 存在时不再判 `person/answer`。

## 8. Effect

```
Effect { kind, key, value(JSON), seq, generation, attempts, error, due, since }   // 主键 (kind, key)
```

见 02-engine §6。

## 9. 外发事件与待人处理

- **外发事件**是内核对外发出的「状态变了」，**不是** Watch 盯的事件，永远不进 inbox、不成为 Trigger、不进研判：`{rev, type, at, ref, data}`。与状态变更同事务落盘，提交后进程内广播；跨进程按 `rev` 补拉。只用于通知与展示，不得驱动领域逻辑。
- **待人处理清单** `attention()`：纯函数，从当前状态现算，处理掉就自然消失。每项 `{id, kind, severity, ref, title, detail, actions[], at}`，kind 为 `answer|review|blocked|failed|delivery|unavailable|suggestion|health`。

## 10. 存储

- 单个 SQLite 文件，WAL 模式，Watch 的表加前缀 `watch_`，只由引擎写。
- 聚合存为 JSON 文档（`value JSON`），用生成列 + 部分索引支撑「活跃 Thread」「在办 Work」查询；引擎代码不拼 SQL。
- 表：`watch_trigger`、`watch_disposition`、`watch_thread`、`watch_work`、`watch_decision`、`watch_trigger_thread`、`watch_inbox`、`watch_effect`、`watch_event`、`watch_kv`；闹钟 `alarm_state`；扩展私有游标 `ext_state`（只放游标等少量状态）。
- 保留期与清理由引擎命令 `prune` 在单事务内完成，受引用保护：open Work、未满保留期的 Work/Decision 及其引用的 Trigger/Thread、非空 `pending/evaluating`、未完成 effect 都不得删。
