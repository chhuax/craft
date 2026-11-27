# 02 引擎、闹钟与 effect

状态：v1 草案。

## 1. 引擎结构

引擎是纯库（`internal/engine`，只许标准库）：同步、无 IO、`now` 注入。组成：

| 部分 | 职责 |
|---|---|
| Intake | 去重、范围过滤、Thread 关联、订阅分发、未认领进 pending |
| Evals | 攒批开研判、决策落地、重新研判 |
| Works | 建/恢复/结束、input、唤醒、订阅增删、attach、handback |
| Assignments | 开始、报告、中断、轮次结束、投递回执 |
| Replies | 回复与通知（经 Channel 端口） |

- **每个公开命令 = 一个事务**。`evaluateDue` 例外：每个 Thread/Work 各自一个事务。
- **事务内必须重读**聚合；事务外读到的对象只用来挑选要处理哪些。
- 领域事件经 `emit` 暂存，事务提交后才发出；回滚则不发。同时在事务内写入 `watch_event`。
- 端口方法在命令事务**里面**被调用，所以真实实现只能写 effect 行或内存登记，不得做 IO。

## 2. 按角色收窄的命令面

调用方只拿到自己角色的接口对象，CLI 子命令按角色分组。

| 角色 | 可调命令 |
|---|---|
| 扩展 | `receive`、`bindThread`、`threadClosed`、`threadReopened`、`describeThread`、`patchThreadMeta` |
| 研判方 | `candidate`、`decide`、`attach`、`reply`、`retryWork`、`reevaluate`、`unsubscribe`、`reassessEval` |
| 执行方 | `assignmentStarted`、`prepareDelivery`、`inputsDelivered`、`inputsRead`、`turnEnded`、`deliveryFailed`、`assignmentInterrupted`、`report`、`markUnrelated`、`handback`、`followForContext`、`reply` |
| 人 | `requestItem`、`stopWork`、`retryWork`、`reevaluate`、`unsubscribe`、`dismiss`、`attach`、`answer` |
| 设置 | `policyChanged` |
| Pass | `evaluateDue`、`prune` |

## 3. 主流程

```
receive(T) ─► inbox
              │ 引擎 goroutine 取出
Intake ─ 去重/过滤 ─┬─ 主 Thread 或其他 Thread 有活的订阅 Work
                    │      → Work.inputs += T；wake → effect(delivery)
                    ├─ followup_only 且无匹配订阅 → filtered 或 recorded
                    └─ 入口事件无人认领 → Thread.pending += T
evaluateDue ── 每个 Thread：无 evaluating 且 pending 非空
               → Policy.gate 逐条准入；无可准入 → ignore
               → 一次取走全部 pending 成一批 → evaluating → effect(assessment)
decide ─────── accept → 建 Work（Mandate）、订阅主 Thread、Assignment(pending) → effect(launch)
               attach → 挂到已有 Work
               ignore / stop / unavailable → 记 Decision
Executor ───── 开会话 → assignmentStarted → 执行方干活 → report
               新事件经订阅进 Work → effect(delivery) 按条依次追加
外部对象关闭 ─ threadClosed → 满足 follows 规则 → Work 结束 → 退订
```

### 3.1 Intake `receive`

返回 `stored | duplicate | filtered | recorded`。

1. `at` 规范化；`threads` 为空是编程错误。
2. 同 id 已存在 → `duplicate`，不改任何东西。
3. 范围过滤（扩展声明的 `scope`）不监听 → 保存 trigger 与 `filtered` 处置。
4. 保存 trigger；对每个 ref 取或建 Thread，更新 `lastAt` 与 `correlated`；计算到达集合 `reached`：
   - Work 不存在，或已结束且不满足「恢复」条件 → 跳过；
   - 恢复条件：`human` 且 `ended.by != "human"` 且该 Work 不是纯 context 跟随。
5. 对 `reached` 的每个 Work 追加 input（id = `key("input", workId, "trigger", triggerId)`）。
6. 保存处置；无人认领且非 `followup_only` → 进主 Thread `pending`；对 fanout 的 Work `wake`。
7. 发 `trigger.received`。

`followup_only` 事件无匹配订阅者时默认记 `filtered`；事件声明 `on_unmatched=record` 则仅记录到最近动态，不追加 input、不唤醒、不进 pending、不建单。

### 3.2 研判与攒批

- 同一 Thread 研判进行中再来的事件留在 pending，下一轮一起判；不设静默期。
- 研判前：pending 中已有 worker 在管的事件移出并发 `trigger.followup`；已开过 Work 的事件直接 ignore。
- 逐条 `Policy.gate`；可准入的选「最强」（strength 更强者胜，同强度取 `at` 较晚者）。
- **v1 研判只做规则版（不借宿主开会话）**：`explicit` 事件按 Policy 的 `auto` 模式直接 accept 建单；`inferred` 事件一律按 `observe` 处理，只记「本会接」的 Decision，不建单。`Assessor` 端口保留，v1 的实现是内置规则；借宿主开一次性会话做研判与选址放 v1.1，到时只替换端口实现，引擎不变。
- 研判超时由应用层处理：放回 pending 队首，连续 3 次记 `unavailable`。
- `Candidate` = 这批 Trigger + 主 Thread（含 facts）+ 该 Thread 上所有 Work + `related`（所有仍 open 且不在前者里的 Work，按创建时间倒序最多 8 个，只给 `{id, title, stage, 自有 Thread key, facts}`），研判方据此判断「一起处理」并 `attach`。相似度怎么判断是研判方的事，引擎不算。
- 卡片有大小上限（trigger 4000 字、窗口消息 400、报告 600、派工 1500），测试守预算。

### 3.3 `decide`

1. trigger 不存在 → `unknownTrigger`；`outcome.evalId` 与当前研判不符 → `evalFinished`。
2. accept 且该 trigger 以前已开过 Work → 返回旧 Decision。
3. 非 stop 且已有 worker 在管 → `workerDecides`。
4. 策略版本变了 → 重排。
5. accept 时调 `Policy.admits(evaluation, rules)`：`ok` → 建单；`observe` → 记 `ignore(observed)`，审计里带 `would_accept` 与 `proposal`，不建单；`refuse` → 抛 `notExplicit`，事务回滚、研判不结束。
6. `accept`：`workId = key("work", evalId)`，`mandate` 不可变，订阅批内全部 Thread，对可关闭的主 Thread 建 own Follow，`launch`。

### 3.4 `attach`

研判方判断这批事件属于某个已有 Work，调 `attach(workId, triggerId, note)`：

- **不另授权**：Mandate 不变，attach 只是路由。人停止的、授权已失效的 Work 不能被挂。
- 事件成为该 Work 的 input，主 Thread 成为它的 own Follow（受单自有对象互斥约束）。
- 执行方可 `handback(triggerId, reason)` 退回；退回时撤销该事件自动建立的 Follow，事件回到研判。
- 已结束（非人停止）的 Work 被 attach 时按恢复规则恢复。

### 3.5 Thread 关闭

`threadClosed(ref, outcome, at, detail?)`：先记对象本身的关闭事实（按「最后状态事实时间」比较，旧结果忽略），再对每个 open Work 标记 Follow 关闭；满足 `followsDone` 且未被 `unfinished` 拖住则结束 Work。`threadReopened` 对称，清 `closed` 并记 `reopenedAt`（即使此前没有关闭记录也记，使之后迟到的旧关闭被忽略）。

## 4. Policy

带版本的纯规则对象，引擎在命令里调用，不做 IO。

```
Policy { version, paused, evalTimeoutMs,
         gate(trigger, thread, works) -> GateResult,     // 谁能发起工作
         revalidate(mandate) -> bool,                    // 恢复已结束 Work 前重查
         admits(evaluation, rules) -> ok|observe|refuse }
```

### 4.1 设置

```jsonc
{
  "paused": false,
  "allowedSenders": [ { "source": "x", "id": "u123", "name": "…" } ],   // 放行的发送人
  "events": { "<source>:<event>": { "mode": "off|observe|auto" } },     // 覆盖扩展声明的默认值
  "rules":  [ { "id": "r1", "name": "…", "enabled": true, "instruction": "…" } ]
}
```

- **事件目录**来自所有已加载扩展的 `watch_sources[].events`，另加内核内置的人事件（见 01 §3 的 `craft:person.*`，不可配置）。用户配置只能改 `mode`。
- **生效规则集** = 事件目录里的默认规则（按事件键排序）+ 用户 `rules`（同 id 以用户为准，其余按原序追加）。`admits`、`revalidate`、研判、worker 消息里的命中规则、`policyEffects` 都读生效规则集。
- **版本** `version = key("policy-v1", canonicalJSON({allowedSenders, events(合并默认值后的 mode 表), rules(生效规则集)}))`。`paused`、超时、`on_unmatched`、`feed_tier`、`verb`、`no_target`、`collapse` **不进**版本，改它们不触发重排。
- **生效**：状态库 `watch_kv["policy.settings"]` 保存**上一份生效的设置全文**（连同版本号），不只存哈希——daemon 重启后要用旧设置算 `policyEffects`。读到的新版本与之不同 → 用旧、新设置计算 effects → **同一个事务里**执行 `policyChanged(old, new, effects)` 并写入新设置；任一步失败整体回滚，下次再试。daemon 启动时也做一次。

### 4.2 gate（谁能发起工作）

```text
gate(trigger, thread, works):
  ev = catalog.get(trigger.source, trigger.event)
  if ev is None:                                  → 拒绝 "unknownEvent"
  mode = modeOf(ev)
  allowed = 在 allowedSenders 里找 (actor.source, actor.id)
  participating = thread 有任何订阅，或 works 不在 thread.context 里
  base = {source, event, sender?: allowed.name}

  1)  trigger.human 且 trigger.addressed 且 ev.strength == "explicit" 且 mode != "off":
        → 准入 explicit，basis {via: "addressed", mode, ...base}
  1a) 非人事件且 ev.strength == "explicit" 且非 followup_only 且 mode != "off":
        → 准入 explicit，basis {via: "event", mode, ...base}
  2)  participating 且 mode != "off" 且 not (trigger.addressed == "direct" 且 not allowed):
        → 准入（explicit 当且仅当 allowed 且 trigger.human，否则 inferred），basis {via: "followup", ...base}
  3)  not ev.followup_only 且 mode != "off":
        → 准入 inferred，basis {via: "event", mode, ...base}
  4)  否则 → 拒绝（followup_only 为真则 "followupOnly"，否则 "modeOff"）
```

**`off` 是人对这一类事件明确关掉，任何人都不能用它发起或恢复工作**（包括放行发送人）。已在办 Work 的后续消息走订阅分发、不经 gate，不受影响。

### 4.3 admits（accept 能否落地）

```text
admits(evaluation, rules):
  basis = evaluation.basis
  mode = 当前 modeOf(basis.source, basis.event)
  if mode == "off":                                            → refuse
  if evaluation.strength == "explicit":
      if basis.via == "followup":                              → ok       # 放行的人在已有工作的对象上明确提出，不受观察模式影响
      return observe if mode == "observe" else ok
  # inferred
  if mode == "observe":                                        → observe
  if rules 非空 且 每个 id 都在设置里且 enabled                  → ok
  否则                                                          → refuse
```

### 4.4 revalidate（恢复已结束 Work、attach 到已结束 Work 前）

```text
revalidate(mandate):
  ev = catalog.get(basis.source, basis.event)
  if ev 不存在 或 modeOf(ev) == "off":                          → false
  if basis.sender 且发送人已不在 allowedSenders 且 basis.via != "addressed": → false
  if mandate.strength == "inferred":
      return modeOf(ev) == "auto" 且 basis.rules 全部仍 enabled
  return true
```

### 4.5 改版影响

`policyEffects(openWorks, old, new) -> map[workId]PolicyEffect`：规则被禁用 → `{change: disabled}`；规则内容变了 → `{change: changed, instruction}`；旧策略能 revalidate 而新策略不能：若把 mode 与规则换回旧值仍不能 → `requesterAllowed=false, requester`；否则 inferred 且 mode 离开 auto → `proactive=false`。受影响的 Work 收到 `policy-changed` input；进行中的研判重排。

### 4.6 其他

- `paused` 时只处理人工请求，自动事件留在 pending。
- **与对外写确认的关系**：Policy 只管「接不接活」。对外写确认属于 outbound（见 04），不进 Watch。

## 5. 闹钟

独立的内核通用服务，不认识扩展与 Watch，只接收调用方登记的 `AlarmSpec{id, every|at, jitter, timeout, run}`。

| 规则 | 说明 |
|---|---|
| 声明 | 扩展 manifest 的 `alarms`；`every`（间隔）或 `at`（每天定点，本地时区）二选一；`jitter` 打散；`timeout` 超时算失败；`default_enabled` 默认 true |
| 覆盖 | 用户配置可覆盖 `every`、`enabled`；`enabled=false` 只停周期调度，不禁止显式手动触发 |
| 不重叠 | 同一 alarm 上一次没结束，本次跳过并记 `skipped`，不排队堆积 |
| 并发 | 每个扩展按 `alarmLimits.concurrency` 限制；全局另有总上限 |
| 失败 | 报错或超时 → 连续失败计数 + 退避（2s、10s、1m、5m 封顶）；连续 N 次进健康告警，成功一次清零 |
| 立即触发 | 扩展或人可请求立即跑一次，仍受不重叠与并发约束；跨进程经 `watch_alarm_request` 表入队，daemon 每 ≤2s 消费 |
| 停止 | daemon 退出时发停止信号（取消 context），等待至多 N 秒 |
| 可测 | 时钟注入，测试拨时钟即触发 |

内核自己的周期任务（记录清理、统计）也用同一闹钟登记，没有特例。

## 6. effect 队列

唯一的副作用队列。主键 `(kind, key)`。

| 机制 | 说明 |
|---|---|
| 同事务入队 | 引擎命令里 `enqueue(kind, key, payload)`，与状态变更同生共死 |
| 覆盖合并 | `(kind, key)` 冲突即覆盖负载、`generation+1`、保留排队位置；同一 Work 连来几条消息只投一次 |
| 并发上限 | 每个 kind 一个 `limit()`，现算；0 即暂停 |
| 重试 | 失败按退避重排；`Later` 表示「还不行」，延后但不计失败 |
| 有序 | `ordered` 的 kind 严格按 seq 一个个跑，失败挡住后面的 |
| 代次 | 执行中的旧代次完成时不删除新覆盖进来的行 |
| 回报 | 执行结果以引擎命令交回，不在 effect 里直接改状态 |

| kind | key | 执行 | limit |
|---|---|---|---|
| `assessment` | evalId | 研判与选址 | 1 |
| `launch` | assignmentId | Preparer 准备目标 → Executor 开/续会话 | 空闲槽位数 |
| `delivery` | workId | 把 undelivered 按条依次追加给执行方 | 4 |
| `reply` | messageId | 回复经 outbound 外发 | 由 Channel 决定 |
| `stop` | assignmentId | 结束会话轮次 | 不限 |

### 6.1 实现要点

- **seq 来自库内计数器**：同一事务里把 `watch_kv["effect.seq"]` 加 1 后使用（单调递增、跨进程安全）。**不得**用进程内计数或 `MAX(seq)+1`——后者在行被删后可能复用旧 seq，让执行中的旧代次误删新行。冲突覆盖时保留旧 seq（排队位置不变）。
- **三种写回（删除、`Later`、失败）都必须带 `WHERE kind=? AND key=? AND seq=? AND generation=?`**；匹配不到（执行期间被覆盖成新代次）就什么也不做。旧代次的失败不能推迟新代次。
- **重试**：失败 `attempts+1`，`error` 截 1000 字，`due = now + min(retryMax, retryBase * 2^min(attempts, 16))`，其中 `attempts` 是**本次执行前**的值（第一次失败等 `retryBase`）。`Later` 只推 `due = now + retryBase`，不加 attempts。
- **调度**（`kick` → `dispatch`）：对每个已注册 kind，`ordered` 取队首一行；否则取 `due <= now` 的前若干行；`room = limit() - len(running)`；依次提交给执行池。`dispatch` 本身出错只把该 kind 健康置 error，不影响其他 kind。每次执行完再 `kick` 一次，腾出的名额立刻给后面的。
- **健康**：每个 kind 一个健康项（等待行数、最早一行时间、最大 attempts、最近错误）；最大 attempts ≥ 3 且非 `quiet` → error，经 `health/report` 进入待人处理清单。
- **停止中**：正在跑的行保持原样，下次启动重试。
- **队列是消费即删**：执行成功即删；`watch_effect` 与 `watch_inbox` 之外的 Watch 记录由 `prune` 按保留期清理。

**槽位**：占槽 = 开会话中（直到宿主确认启动）、一轮进行中；交给人（报 `ready/blocked/failed`、有 `attention`）后立即释放；人的消息可额外占 1 个槽。等槽就是 launch 行排在队里。launch 默认按 seq 排序，Policy 可提供 `launchOrder(work)`，只按约定 facts（`urgency`、`due`）排。

## 7. 线程模型与 Pass

引擎 goroutine 是唯一跑 Pass 的地方：取 inbox → `receive` → `evaluateDue` → `effects.kick`。inbox 的事务规则、唤醒来源、跨进程与停止顺序见 `06-runtime.md` §2、§3。

## 8. 执行方、准备器与会话

端口 `Executor`、`Preparer`、`Assessor`；每个 Work 一个执行会话，之后的 Assignment 复用并只送增量。**会话历史不是事实来源**：状态在库里，执行方随时 `craft watch read` 重读，宿主压缩或丢上下文不丢事实。launch、槽位、投递、未知结果、重启恢复、代次与停止顺序、写入边界等见 `07-executor.md`。

- `Assessor { request(candidate); release(evalId) }`：v1 的实现是内置规则（见 §3.2）。
