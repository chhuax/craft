# 07 执行方与会话

状态：v1 草案。规定 Work 被接下之后，怎么交给执行方（本地 AI 编码助手会话等）去做、怎么跟进、出错怎么办。内核不写宿主名分支，宿主差异由 Executor 适配层与宿主 manifest 的能力声明处理。

## 1. 端口与 effect

```
Executor { start(work, assignment); deliver(work, assignment); stop(work, assignment, reason) }   // 只排队，结果经引擎命令回
Preparer { prepare(target, work) -> PreparedTarget }
```

- `start` → `enqueue("launch", assignmentId, {workId, assignmentId})`；`deliver` → `enqueue("delivery", workId, ...)`；`stop` → `enqueue("stop", assignmentId, {workId, assignmentId, session})`。
- 注册：`launch`：`limit = paused ? 0 : slots.launches()`，退避 `(5s, 5min)`；`delivery`：`limit = paused ? 0 : 4`，`quiet`；`stop`：`limit = 4`。
- 执行方回报全部是引擎命令，且都带 `assignmentId`（见 §10 代次）。

### 宿主能力声明（宿主包 manifest，内核只读）

| 能力 | 含义 | 缺失时的降级 |
|---|---|---|
| `createSeparatesPrompt` | 建会话与发首条是两步（先拿会话号再发）；为 false 表示可以建会话时直接带首条 | 按能力选择路径，见 §2 |
| `busyAppend` | 会话忙时能追加消息 | `deliver` 返回 `Later`，等轮次结束再投 |
| `stopHook` | 有轮次结束信号（Stop hook） | 靠轮询 `sessionStatus` 判断 |
| `sessionStatus` | 能查会话状态 `busy/idle/gone/unknown` | 视为 `unknown`，重启后交人核对 |
| `lastReply` | 能取会话最近一条回复 | 提示人打开会话查看 |
| `workspaceBoundary` | 宿主能否把会话限制在某个目录 | 依赖 §12 的 guard |

适配器做不到某项能力时返回 `UNSUPPORTED`，按降级处理；不得绕过校验。

## 2. launch

```
launch({workId, assignmentId}):
  work = 读; a = latest assignment
  work 不存在/已结束/work.delivery 非空/a.id != assignmentId/a.state != pending → 直接返回（不报错）
  target 为空 → launchFailed("没有执行目标")   # 不得静默返回：Work 会永远停在 queued，界面上看不出原因
  该 Work 有未完成的 stop effect → 抛 Later（先等旧会话停下）
  slots.reserve(workId, "launch")            # 没空槽 → Later
  try:    launchImpl(work, a)
  except Later / 正在停止: 原样抛
  except PrepareFailed: launchFailed(...)     # 不自动重试
  except 短暂错误 且 重试次数未满: 抛（effect 退避重试）
  except 其他: launchFailed(...)
  finally: slots.free(workId)

launchImpl(work, a):
  current() = 未在停止 且 未暂停(否则 Later) 且 work 仍 open 且 latest == a
  1. 目录：execution.directory 已存在则继续用；否则 Preparer.prepare(target)。准备失败 → PrepareFailed（空目录删掉，非空保留）。目录变化时记 execution.directory。
  2. 会话：
       无会话或 sessionStatus == gone：
         createSeparatesPrompt=false → 建会话时直接带首条和投递 ID，回调先登记，OK 后记 bootstrapped=true
         createSeparatesPrompt=true  → 发空引导建会话，登记后再追加首条
       有会话且 bootstrapped=false → 补发首条
       有会话且 bootstrapped=true  → 追加本 Assignment 的增量消息
       忙且不 busyAppend → Later
       建会话 UNKNOWN → 按 §7
  3. 每步之间 current() 为假则返回
  4. engine.assignmentStarted(workId, assignmentId)
```

- `launchFailed`：调 `engine.report(failed, summary, assignmentId)`，由面板展示。
- **执行目标（v1）**：v1 没有选址（选址放 v1.1），所以 **accept 建 Work 时，没有预设目标的一律设为 `scratch` 目标**（`TargetSnapshot{kind: "scratch", selectedBy: "default"}`，`evidence` 写明「v1 无选址，默认在临时目录执行」），不论来源是人的请求还是扩展的事件。Preparer 为它创建 `<状态目录>/scratch/<workId>`（权限 0700、不在任何代码仓库里、已存在则复用、**绝不删除非空目录**），会话在该目录里启动。这与 03 §1 事件目录里 `no_target: "scratch"` 的含义一致；该字段在 v1 是冗余的。临时目录的清理归 `prune`。把 Work 落到某个仓库的 worktree 是 v1.1 的选址。
- 因此 launch 看到空的 `Execution.Target` 只会出现在损坏或旧数据里，必须报告失败而不是等待。
- 短暂错误（宿主 `UNAVAILABLE`，或网络、限流、5xx）最多重试 3 次（含首次共 4 次），超过记失败。
- **git 之类的目标准备必须由 Preparer 完成**，且要先走宿主的目录规划（有些宿主对工作目录有命名或位置要求）；不得绕过规划直接用默认路径。

## 3. 槽位

`WorkerSlots(max)`：`used()`、`launches()`、`reserve(work, by)`（`person` 可多占 1 个；无槽抛 `Later`）、`free()`。

- 占槽：开会话中（直到宿主确认启动）、一轮进行中（`running(work)` = 有 running assignment 且监视状态为 `busy` 或处于「启动确认中」）。
- 不占槽：交给人（最新报告 `ready/blocked/failed` 或有 `attention`）后立即释放。
- **不搬**「一轮结束后后台任务继续跑仍占槽」：内核看不到宿主的后台任务。
- `max` 默认 3，可配置（`watch.executor.maxWorkers`）。
- **启动确认中**：`create_session` / 首条追加返回 `OK` 时，监视立刻把该会话记 `busy`（不等下一次采样），所以 `slots.free()` 之后 `running()` 仍为真，不会多开。

## 4. 给执行方的消息

- **首条**（每个会话的第一次派工）：`{work_id, title, assignment(≤1500 字，超出给 read), trigger: 卡片(≤600), batch, target: {path}, read_first: <执行手册路径>}`，渲染为 Markdown。结构分层：**要做的事**（接活说明：事件的默认规则或命中的规则正文；范围）、**在哪做**（任务副本目录；与所属仓库不同时另列主检出并注明不要在那里改）、**怎么做**（执行手册、扩展规则、`craft watch read/report`）、**事件原文**（引用块 + 来源人与时间、链接、同批其余事件）。内部 id 只放最后一行「引用：」。
- **会话标题**：「动作 · 短编号 短标题」，不加品牌前缀。动作取事件目录 `verb` → Thread 事实 `item_type` 的标签 →「处理」；短编号取 Thread 事实 `short_id` → 对象编号最后一段；短标题取事实 `title`，去掉提交前缀与开头标签，超 16 字截断；一个派工合并多个对象时加「等 N 项」，挂上或退回对象后经 `rename` 同步。
- **后续 Assignment**（重试、恢复、唤醒）在会话还在且首条已确认时，只发增量 `{work_id, assignment: 序号, note?}`；会话重建后先发首条。
- **输入消息**：trigger → 卡片（≤600 + read + 路由说明 + 回复目标）；`policy-changed` → 策略变化说明；`resumed` → 恢复说明；`note` → 「主会话转告（仅供参考）：…」；`text` 原样。
- **合并输入**：批内各条按输入时间升序（同时间按队列顺序），一次追加。
- **批内合并同类**：事件在 manifest 声明 `collapse` 时，同一批里合并键相同的 trigger 只渲染最后一条并注明合并了几条；被合并的输入照样记送达，原文仍可 `read`。合并键 = （来源、`collapse.group`、首个 Thread、是否仅作上下文），声明了 `collapse.by` 再加 `meta[by]`（事件缺这个字段就不合并）。没声明的事件与非 trigger 输入一律不合并。

## 5. delivery

先等该 Work 的停止完成，只投给 running assignment。每次从 `undelivered` 队首取一条或一批，**每次发送前重读 Work**。

- **外部事件按静默窗口攒批**：来源不是本机的 trigger 输入（包括外部系统里人写的评论，评审人常连发多条）都攒批。从队首这类输入起攒；每到一条新的，到期时间推到「最后一条入队时间 + 静默窗口」，但不晚于「队首入队时间 + 最长等待」。到期后把队首起连续的这类输入一次发送。默认静默窗口 300 秒、最长等待 900 秒；静默窗口设 0 恢复逐条即时投递。到期时间用 effect 的持久化 `due` 调度，daemon 重启后继续。
- **本机人的输入立即送达**：内核内置来源的输入（人工回复、人工发起）和人的文本输入立即推动队列发送，可带上队首已攒的外部输入一起。
- 空闲时先 `slots.reserve`；批内含本机人的输入时用 `person` 占槽。会话丢了按 §2 共用建会话及首条流程，**首条确认后才追加输入**。
- 批量追加前记录队首批次的输入 id，重试时保持批次成员和投递 id（§6），后来入队的输入留待下一次。
- `OK` → 一条引擎命令 `inputsDelivered(workId, assignmentId, inputIds, read=true)` 在同一事务里记本次所有输入的送达与 `readAt`，并清除批次记录。
- 忙且不 `busyAppend` → `Later`；`UNAVAILABLE` → `Later`；`UNKNOWN` → §7；其他失败 → `engine.deliveryFailed(...)` 后抛（Work 进待人处理 `delivery`）。

## 6. 投递 ID

- 逻辑请求 id（`assignment:<work>:<assignment>`、输入 id 等）只在内部用（effect key、去重）。单条输入沿用 `input.id`；多条输入为 `key("delivery-batch", sorted(inputIds)...)`，由该批输入集合确定。
- 交给宿主的 `deliveryId = "w" + key(requestId)`（32 位十六进制，满足 `[A-Za-z0-9_.-]+`）。同一逻辑请求永远得到同一个投递 id；**执行方的规则是「同一投递标记只处理一次」**，据此去重。
- 首条逻辑 id 为 `assignment:<work>:<assignment>:init`，直带、补发和同一派工重建会话共用；增量为 `assignment:<work>:<assignment>`。
- 带投递 id 建会话时 id 不合法则返回 `UNSUPPORTED`。测试的假宿主必须走真实的标记函数，不能绕过校验。

## 7. 宿主「结果未知」（UNKNOWN）

| 操作 | 处理 | 理由 |
|---|---|---|
| 建会话 | 未回调登记会话号 → 首条未发，**自动重建一次**；仍未取得且结果未知 → `report(failed, "宿主没有确认会话是否已建")`。直带模式已回调 → 首条结果未知，按「追加」规则记 `deliveryFailed` 交人核对。两步模式已回调 → 查 `sessionStatus`，存在则继续发首条，查不到则重建一次 | 直带模式拿到会话号后可能已发送；两步模式在建会话阶段只可能运行空引导 |
| 追加（首条与后续输入） | **不重试、不当送达**：同一投递 id 只发一次；`UNKNOWN` → `deliveryFailed(原因 + 宿主给的核对办法)`，输入留在 `undelivered`，首条不记 `assignmentStarted`，Work 进待人处理 `delivery` | 回执丢失时可能根本没送到（执行方空等），也可能已送到（重发多跑一轮），只有人能去宿主里核对 |
| 停止 | 重试至多 3 次，之后视为完成并记健康告警 | Work 在领域里已经结束，旧会话最多多跑一会儿 |

`delivery` 卡片动作：

- `retry`：**原地重发**——不结束派工、不建新代次，在原会话用**同一投递 id** 重发（首条未确认时重走该派工的 launch 补发首条）；执行方按投递 id 去重。
- `markDelivered`：人核对后确认已送达：首条记 `assignmentStarted`，后续输入 `inputsDelivered(read=true)`，清掉投递失败。合并批次确认原批全部输入，随后继续投递其余输入。
- `stop`。

## 8. 会话监视与轮次结束

- 闹钟登记 `watch.sessions`（每 10 秒）：对每个 running assignment 的会话调 `sessionStatus`，结果存内存（供 `running()`）并与上次比较。
- **有 Stop 钩子的宿主**：钩子调内部命令 `craft watch turn-ended`（带宿主会话号），**直接**执行轮次结束处理，不依赖下一次采样（短轮次不会漏）；轮询只兜底。
- **无钩子的宿主**：靠采样；`idle` 且「最近一次投递晚于最近一次 `idleAt`」→ 视为一轮结束（即使两次采样都是 idle 也能补上）。
- `busy → idle` 视为一轮结束，执行 `workerIdle`：
  1. Work 仍 open 且该 assignment 仍在运行，否则返回；
  2. 「已交给人」（最新报告 `ready` 或有 `attention`，且之后没有新投递；或 `waitsForPerson`）→ `engine.turnEnded`，结束；
  3. 否则 `engine.turnEnded`；若 Work 仍 open 且本轮没有新报告 → 带当前派工代次调用 `engine.report(waiting, summary)`，摘要优先取 `lastReply`。
- `→ gone`：running assignment 的会话没了 → `engine.assignmentInterrupted`。
- **首条也算投递**：`assignmentStarted` 同时记 `deliveredAt`（让「最近投递晚于 `idleAt`」的补偿条件对第一轮也成立）。
- **本轮起点**：发首条或追加增量**前**写 `watch_kv["launch.<assignmentId>"] = {at}`；同一逻辑投递重试保留第一次调用的起点；`assignmentStarted` 或该次 `inputsDelivered` 提交时把起点记为 `deliveredAt` 再删记录。
- **Stop 先于投递回写到达**：`turn-ended` 若找不到 running Assignment，或当前会话的唤醒投递还未回写，就把 `{session, at}` 暂存到 `watch_kv["turn_end.<session>"]`；`assignmentStarted` / `inputsDelivered` 提交后检查暂存，`at` 不早于本轮起点就按上面处理，然后删除。daemon 启动时遍历 `turn_end.*`：投递仍待回写则等回写；有 running Assignment 且 `at` 不早于其 `deliveredAt` 则补处理；其余直接删除。

## 9. 重启恢复

daemon 启动、effect 开始前，按 `createdAt` 升序遍历 open Work：

- **有 running assignment**：查 `sessionStatus`。`gone/unknown` → `assignmentInterrupted`（已有待执行的 `delivery` effect 时由该 effect 到期后处理会话重建；否则：最新报告已是 `waiting/ready/blocked/failed` 且之后没投递 → 保留会话、补投 `undelivered`；否则新 Assignment + 恢复说明，提示「宿主在这项工作进行中重启过，继续前先检查当前目录、版本记录和外部系统状态，已完成的对外操作不要重复做」）。`idle/busy` → 保持，交给监视。已入队的合并 effect 按持久化的到期时间继续。
- **最新是 pending 且已有 target**：`work.delivery` 非空时继续等人核对；没有投递失败且不在等人时：
  - 没有未确认首条的本宿主会话 → `executor.start`；
  - 已登记会话且 `bootstrapped=false`：直带宿主查会话状态；两步宿主没有 `launch.<assignmentId>` 起点时直接入队发首条，有起点时也查会话状态；
  - 状态 `gone` → 清会话登记并入队重建；`busy` → 补记 `bootstrapped=true`、以本轮起点记 `assignmentStarted`，监视记为忙并处理暂存 Stop；`idle/waiting_person/unknown` 或查不到 → 记 `deliveryFailed` 交人核对。

## 10. 派工代次与停止顺序

- **代次就是 Assignment id**。执行方所有回报命令（`assignmentStarted`、`inputsDelivered`、`turnEnded`、`deliveryFailed`、`setExecution`）都带 `assignmentId`，引擎在事务内核对它是最新且未结束的，否则忽略。
- **执行方命令按会话识别**：换会话后，旧会话迟到的 `report` 因会话号不再对应任何 open Work 的会话而被拒（`noWorkForSession`），不会写到新 Assignment 上。
- **stop 载荷**：`{workId, assignmentId, session: {host, id}}`，不依赖 Work 当前的 `execution.session`（可能已清空）。
- **先停后开**：同一 Work 有未完成的 stop effect 时，`launch` 与 `delivery` 都抛 `Later`。
- **retry 复用会话**：结束旧 Assignment → stop（停当前一轮）→ 停止完成后 launch 往同一会话发增量；**换目标**：stop 旧会话 → 清会话记录 → 指定新目标后 launch 新建会话。
- 每次真正调宿主前（追加、建会话）再读一次 Work，确认 Assignment 仍是最新且未结束。
- `launchFailed`、自动写的 `waiting` 报告同样带代次；`assignmentId` 不是最新、已结束或 Work 已结束 → 忽略。
- **建会话途中换了代次**：`on_created` 调 `setExecution` 返回登记失败 → 不发首条并返回 `UNKNOWN`；拿到的会话号随即入队 stop。
- **stop handler**：`stopSession(session)`；`UNAVAILABLE` → `Later`；其他结果都算完成（Work 在领域里已结束）。

## 11. 执行方命令

只在执行方会话里可用，都要求角色识别为执行方且作用于自己的 Work；Work 已结束 → `workEnded`。

| 命令 | 输入 | 作用 |
|---|---|---|
| `craft watch report` | `{state: progress\|ready\|blocked\|failed\|dropped, summary, humanAction?, ask?, actionId?, questionForTriggerId?, replyToTriggerId?}`。**不能报 `waiting`**：它只由执行器在轮次结束时写 | `engine.report` |
| `craft watch ack` | `{triggerId, text}` | `engine.reply(acknowledgement)` |
| `craft watch handback` | `{triggerId, reason ≤300}` | `engine.handback` |
| `craft watch thread-unrelated` | `{thread, reason}` | `engine.markUnrelated` |
| `craft watch thread-follow` | `{thread, reason}` | `engine.followForContext` |
| `craft watch wrong-target` | `{reason ≤300, suggestion?}` | 报告选错了目标 |
| `craft watch read` | `{resource, offset?}` | 只能读本 Work 的资源与本 Work 到达过的 trigger |

- `humanAction` 传字符串时创建问题（`ask` 缺省为 `owner`，可附 `actionId`）；传 `null` 清除当前问题；省略则保留。`ready` 的清理规则照旧。
- `summary` 不设长度上限，面板按卡片展示限制截断。
- **角色识别以宿主会话号为准**：取当前宿主会话号与某个 open Work 的 `execution.session` 比对；环境变量（`CRAFT_WATCH_ROLE`、`CRAFT_WORK_ID`）只在取不到会话号时作辅助。两者都没有 → 视为人。
  - **Claude Code**：会话号取自会话 Bash 里的 `CLAUDE_CODE_SESSION_ID`，并**完全忽略** `CRAFT_*` 声明。原因（实测）：`--bg` 会话由常驻的后台服务托管，继承的是服务启动时的环境，其中的 Work、派工号是陈旧值，且没有 `CRAFT_SESSION_ID`。交互式的人用会话里也有该变量，所以必须再按会话号反查 open Work：查到 → 补全为 worker 绑定（取最新派工）；查不到或已结束 → 视为人；多个 open Work 同属一个会话 → 报错，不猜。没有该变量时不查库。
  - **Claude Code 建会话直带首条**：不带正文的 `--bg` 只会建出没有对话记录的空会话，既不能 `--resume` 也没有别的投递入口，所以该宿主声明 `createSeparatesPrompt=false`。

## 12. 执行方的写入边界

「自动派工只改任务副本，写错到人的主检出要拦住」。

- 主检出范围取 Work 的 `execution.target.path` 与本地登记的工作区路径的并集。目标路径落在主检出内且在 `execution.directory` 之外时拒绝。比较前解析真实路径及平台上的大小写和分隔符。任务副本及主检出之外的目录放行（shell 仍按各宿主原有规则）。
- 宿主 hook 取不到会话号时：按 hook 事件的**当前工作目录**匹配某个 open Work 的 `execution.directory` 识别执行方，再判断写入目标是否落在主检出里。**不能按写入目标路径来识别**（写向主检出时恰好匹配不到执行方）。
- **停止进行中的旧会话**：换目标清掉 `execution.session` 时，同一事务把旧会话写入 `watch_kv["stopping.<host>.<session>"]`（`workId`、`directory`、`targetPath`），stop 完成后删除。查询在 Work 里查不到时再查这里，旧会话停下之前仍按原归属判定。
- `stopping.*` 的删除条件：仅当 `stopSession` 返回确定的 OK 或 `sessionStatus == gone` 后；`UNKNOWN` 重试耗尽记完成时**不得删除**。
- 人来源的会话（`mandate.basis.via == "humanDirect"`）放行主检出写入；不占槽位，不自动投递，会话 `gone` 时调 `humanSessionClosed`，绝不调 `assignmentInterrupted` 或重建后台会话。
- 查询函数放 `internal/store` 只读 SQL，guard 从这里读取，不导入 Watch 运行层。

## 13. 测试要求

- 两个 `launch` 并发抢最后一个槽只成功一个。
- 首轮及普通投递唤醒的短轮次在有钩子与无钩子两种宿主上都被识别；重启时已 idle 的会话补做一次轮次结束。
- 同一会话 + **全新的适配器实例**（模拟 daemon 重启）追加消息成功。
- 假宿主的 `UNKNOWN` 路径：建会话、追加、停止各一条用例，验证 §7 表。
- 写入边界：新执行方写自己的 worktree 放行；写人的主检出被拦；写别处放行。
