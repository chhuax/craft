# 06 运行层：存储、inbox、Pass、扩展装载

状态：v1 草案。引擎（02）是纯库，本文规定把它跑起来的运行层。

## 1. 存储与连接

- 单个 SQLite 文件（ADR 0003），WAL 模式。**每个并发使用者一条连接**，不共享写连接；每条连接设 `journal_mode=WAL`、`synchronous=FULL`、`busy_timeout=5000ms`。
  - `synchronous=FULL` 是因为 inbox 承诺「`watch/receive` 返回即不丢」，要扛住断电。
- **写事务一律 `BEGIN IMMEDIATE`**（SQLite 写锁串行）。`atomic(work)` 可嵌套：最外层 `BEGIN IMMEDIATE`，内层用 `SAVEPOINT`；异常时内层 `ROLLBACK TO` + `RELEASE`，外层 `ROLLBACK`。
- `afterCommit(fn)`：最外层提交成功后按注册顺序调用；回滚则丢弃；回调出错只记日志，不影响已提交事务。广播与唤醒都挂在这里。
- 聚合表用生成列 + 部分索引支撑「活跃 Thread」「在办 Work」查询，需要 SQLite ≥ 3.31（JSON 路径 `[#-1]`）。纯 Go 驱动自带的版本满足；CI 里加一条版本断言。
- **表的写入方**：`watch_trigger/disposition/thread/work/decision/trigger_thread` 只有引擎命令写；`watch_effect` 只有 effect 层；`watch_inbox` 只有 inbox（写）与引擎 goroutine（删）；`watch_kv` 只有运行层；`watch_event` 只有引擎 `emit`（追加）与 `prune`（删过期）；`ext_state` 只有扩展状态入口；`alarm_state` 只有闹钟。SQL 只在 `internal/store`。
- **同一套引擎用例必须在内存仓储与 SQLite 仓储上各跑一遍**（参数化测试基类）。内存仓储的 `atomic` 用深拷贝快照实现回滚。

## 2. inbox

```sql
watch_inbox (id TEXT PRIMARY KEY, value TEXT NOT NULL, at TEXT NOT NULL, seq INTEGER NOT NULL)
```

`watch/receive` 的处理（在调用方 goroutine，不进引擎）：

1. 校验并规范化（见 §6）；不合格是扩展自己的 bug，返回协议错误。
2. **一个 `BEGIN IMMEDIATE` 事务里**：`watch_trigger` 已有该 id → `duplicate`；否则 `watch_kv["inbox.seq"]` 加 1，`INSERT OR IGNORE INTO watch_inbox`，按**实际插入行数**判定：1 → `queued`，0 → `duplicate`。查重与插入必须在同一事务，否则与并发 `receive`、与 `drain`（读 inbox → 写 trigger → 删 inbox）交错时会误判。
3. 提交后唤醒引擎 goroutine，返回结果。

**inbox 行落盘后才返回**，所以扩展之后推进游标不会丢事件。

## 3. Pass 循环

```
loop:
  wait(wake, 2s)                  // 走注入的时钟：测试拨时钟即醒
  drain inbox                     // 按 seq 取 ≤100 行；每行：engine.receive(trigger) 与删 inbox 行在同一事务
  engine.evaluateDue()
  effects.kick()                  // 放在 evaluateDue 之后：本轮新入队的 effect 立刻有机会执行
  if 还有 inbox: wake.set()
```

- **只有这一个 goroutine 跑 Pass**；`passes.request()` 只是置位 wake。
- `drain` 里 `receive` 与删 inbox 行在**同一事务**，崩溃后重放既不重复也不丢。
- `receive` 返回 `filtered/duplicate` 只记日志。
- 引擎 goroutine 不得调用扩展、不得做网络或子进程调用。

### 唤醒来源

| 来源 | 做法 |
|---|---|
| `watch/receive` | 置位 wake |
| effect 执行完 | 执行 goroutine 在回报命令提交后置位 wake |
| 同进程的引擎命令（如面板操作） | `afterCommit` 置位 wake |
| 别的进程（CLI）提交了引擎命令 | daemon 每 2 秒用 `PRAGMA data_version` 判断有外部写入，有则立刻再跑一轮 |
| 定时兜底 | 2 秒 |

### 跨进程规则

- 引擎是库：任何进程都可调用引擎命令。
- **effect 只在 daemon 里执行**：CLI 进程只能 `enqueue`，不得启动 effect 执行池。
- 启动顺序：先启动 effect 层，再启动 Pass；停止顺序：停闹钟 → 停 Pass（等当前一轮）→ 停 effect（正在跑的行保持原样，下次启动重试）→ 关库。

## 4. 扩展装载与重载

- **daemon**：启动时扫描并装载扩展一次，登记闹钟、事件目录与各端口；**运行期间不重扫**。
- **CLI 进程**：第一次用到扩展信息时扫一次（进程很短，相当于每条命令读最新 manifest）。
- **变更生效**：沿用「指纹」机制，不另加文件监听。daemon 启动时记下指纹（二进制版本 + 所有扩展 `manifest.json` 的路径、修改时间与大小，不读扩展代码以免变慢），每拍比对；不一致则按上面的停止顺序优雅退出，由新进程接棒并重新扫描。CLI 调用前可 `ensureFresh()` 温和停掉指纹过期的 daemon 并拉起新版（带冷却，防重启风暴）。
- **指纹计算失败**（升级覆盖中途文件不全）时返回空串、**不触发**重载，防重启风暴。这条不要「优化」成报错。

## 5. SourceContext 的执行位置

扩展经协议调用的方法按位置分两类：

- **经 inbox**：只有 `watch/receive`——唯一可能大批量、且后续处理（分发、唤醒）较重的入口。
- **在调用方 goroutine 里直接执行的引擎命令**（短事务）：`thread/describe`、`thread/patch`、`work/bindThread`、`thread/closed`、`thread/reopened`。不经引擎 goroutine、不经 inbox；提交后置位 wake。
- **只读**：`thread/subscribed`、`thread/get`、`work/open` 走只读连接（WAL 下不与写冲突）。

`watch/receive` 的校验：

- `source` 必须等于本扩展声明过的某个 `watch_sources[].id`；`event` 必须在该 source 的事件目录里；
- 每个 `threads[].source` 必须是本扩展声明的 source，`kind` 在 `thread_kinds` 里；
- `id` 非空；`at` 可被规范化；`text` 超过 4000 字符则截断并置 `meta.truncated = true`；
- 传给 `describe/patch` 的 `key` 必须属于本扩展的 source。

`thread/subscribed(kind)`：返回本扩展 source 的、`subscriptions` 非空的 Thread，且满足「有活的 Work」或「`max(lastAt 或 createdAt, 各订阅 Work 的 ended.at)` 在 14 天内」。

## 6. 扩展作者守则

1. Trigger id 必须由外部事件的稳定标识生成（如 `key(source, 外部事件 id)`），同一事件重复拉到 id 相同。
2. 本系统自己做出的操作（自己写的评论、自己改的状态）发出的 Trigger 要么不发，要么 `human=false`，避免自己唤醒自己。
3. 游标在 `watch/receive` 返回**之后**再推进；拉取窗口回退几秒以容忍延迟到达，重复由 id 去重。
4. 对象当前状态（标题、截止、紧急度、状态）用 `thread/describe` 写 facts，不要塞进每条 Trigger。
5. 外部对象不再归我或已关闭 → `thread/closed`。执行方在工作里新建的相关对象（如 PR）用 `work/bindThread(own=false)` 绑定；只有 Work 从该对象起源时它才是 `own`（由 accept 时登记）。**先完成 `bindThread`，再发该对象的「已合并」类事件**，否则事件先被无订阅消费，之后补绑也不会重发。
6. 入口里定期检查取消；不要在入口里 sleep 等待。

## 7. 保留与清理

- 内核闹钟登记一个清理任务（默认每天凌晨），调用 `prune(triggerBefore, workBefore, eventBefore)`。默认：已结束 Work 与 Decision 保留 90 天；`watch_trigger/disposition/trigger_thread` 保留 30 天；`watch_event` 保留 30 天且至少保留最近 5000 条。
- **引用保护**（事务内现算，命中任一条绝不删）：所有 open Work 及其引用的 trigger；保留期内的已结束 Work、独立 Decision 及其引用的 trigger；非空 `Thread.pending/evaluating`、未完成 effect、未终态外发记录引用的对象。
- **级联**（同事务）：删过期 Work 时同步从 Thread 的 `subscriptions/context` 移除；删过期 Trigger 时同步删处置与 `watch_trigger_thread`；删过期事件时更新 `watch_kv["event.min_rev"]`。
- **`watch_trigger` 的保留期必须明显长于所有扩展的拉取回看窗口**（协议 03 §5.1：重放期 ≤ 7 天，清理期 30 天），否则被重复拉到的旧事件失去去重记录会再次入队。

## 8. 原因码

供 Decision 的 `reason` 使用，文案本地化：`said`、`timeout`、`policyChanged`、`noAdmissible`、`startedWork`、`acceptedBefore`、`routed`、`humanStop`、`stopUnknown`、`stopNotExplicit`、`ignoredFromCard`、`observed`、`retried`、`hostRestarted`；gate 拒绝码 `unknownEvent`、`followupOnly`、`modeOff`；scope 拒绝码 `sourceOff`、`unknownSource`。
