# 03 扩展协议

状态：v1 草案。扩展是**独立进程**，可用任意语言实现，与内核经 **JSON-RPC 2.0 over stdio** 双向通信（每条消息一行，UTF-8，`\n` 分隔）。选型理由见 `docs/adr/0002-out-of-process-extensions.md`。

## 1. 目录与 manifest

```
<extensions>/<id>/
  manifest.json
  <可执行文件或启动脚本>
```

```jsonc
{
  "id": "github",                      // 全局唯一；扩展之间禁止互相依赖
  "version": "0.1.0",
  "protocol": 1,
  "exec": ["./craft-ext-github"],       // 启动命令，工作目录为扩展目录
  "requires": [],                      // 依赖的其他扩展 id，运行时经内核协作，不得 import
  "alarms": [
    { "id": "inbox", "every": "5m", "jitter": "30s", "timeout": "2m", "default_enabled": true },
    { "id": "daily", "at": "09:00",  "timeout": "2m" }
  ],
  "alarm_limits": { "concurrency": 1 },
  "watch_sources": [{
    "id": "github",
    "thread_kinds": [
      { "id": "pull_request", "closable": true, "object_type": "github.pull-request" }
    ],
    "events": [
      { "id": "pr.review_requested", "label": "请我评审", "default": "observe",
        "strength": "explicit", "no_target": "scratch",
        "rule": { "name": "评审 PR", "instruction": "…" }, "verb": "评审" },
      { "id": "pr.merged", "label": "我的 PR 已合并", "default": "auto", "strength": "inferred",
        "followup_only": true, "on_unmatched": "record", "feed_tier": "notice" }
    ],
    "facts": ["title", "state", "due", "urgency"]
  }],
  "actions": [
    { "id": "pr:comment", "level": "auto",    "reversible": true },
    { "id": "pr:create",  "level": "confirm", "reversible": false }
  ],
  "ports": { "assessor": false, "executors": [], "preparers": [], "channels": ["github"] },
  "placement_rules": [],
  "views": []
}
```

- manifest 字段一律顶层、snake_case。
- 事件目录项：`id`、`label`、`default`（`off|observe|auto`）、`strength`（`explicit|inferred`）、`followup_only`、`on_unmatched`（`filter|record`）、`feed_tier`（`notice|digest`）、`rule`、`verb`（≤6 字，仅展示）、`no_target`（`scratch`）、`collapse`（`{group, by?}`，投递时合并同类事件）。
- 非法值在加载 manifest 时拒绝。事件类型按 `<source>:<event>` 全局唯一。
- **标识里不许含冒号**：扩展 `id`、闹钟 `id`、`watch_sources[].id`、`thread_kinds[].id` 在加载时拒绝 `:`。内核用冒号拼接这些标识得到库键（`<source>:<kind>:<id>`、`<ext>:<alarm>`、`<source>:<event>`），标识里有冒号会让不同的引用拼出同一个键，权限检查就被绕过。对象自己的 `id`（`ThreadRef.id`）允许含冒号——前面的各段不含冒号时，拼出的键仍可唯一还原。
- **每个 `watch_sources[].id` 只归一个扩展**：一个扩展可以声明多个 source，但两个扩展声明同一个 source 时，**争用的各方都被拒绝加载**（结果不依赖加载顺序，其他独立扩展照常启动）。这是权限边界：扩展只能写自己声明的 source 下的对象（`thread/describe`、`thread/closed`、`work/bindThread` 等），source 被两个扩展共用就等于它们能互相改对方的对象。`craft` 是内核内置人事件的保留 source，任何扩展都不能声明。
- `rule`、`default`、`strength` 进策略版本；`verb`、`no_target`、`collapse`、`feed_tier` 不进。

## 2. 生命周期

1. daemon 启动扩展子进程，发送 `initialize`；扩展回应能力后进入 ready。
2. 内核按需发 `alarm/run`、`action/execute`、`context/read` 等请求；扩展随时可向内核发请求（见 §4）。
3. daemon 退出或扩展停用时发 `shutdown`，等待至多 N 秒后终止进程。
4. 子进程崩溃：内核记健康告警，按退避重启；进行中的请求以 `Unavailable(retryable)` 失败。

## 3. 内核 → 扩展

| 方法 | 参数 | 说明 |
|---|---|---|
| `initialize` | `{protocol, kernelVersion, config, stateDir}` | 返回 `{ok, capabilities}` |
| `alarm/run` | `{alarmId, reason: "schedule"\|"requested"\|"startup", scheduledAtMs}` | 扩展在其中拉外部数据并调 §4 的方法；返回即算完成。收到 `$/cancel` 或超时应尽快返回 |
| `context/read` | `{thread, trigger?, resource?, cursor?, messageId?}` | 读对象的上下文（正文、最近评论等），分页；供研判与执行方现查 |
| `action/execute` | `{action, params, preview: bool, idempotencyKey}` | 对外写。`preview=true` 只返回预览不执行（见 04） |
| `action/verify` | `{action, idempotencyKey}` | 崩溃后核实某次对外写是否已生效 |
| `assess/request` / `executor/start` / ... | — | 仅声明了对应 `ports` 的扩展实现；内核只排队，结果经 §4 或 CLI 命令回 |
| `shutdown` | `{}` | — |

取消：内核发 `$/cancel {id}` 通知，扩展应尽快返回；超时后内核仍标记失败，进程留着等其自行结束，期间不再调度该 alarm。
实现上，内核对 `alarm/run` 的调用在超时/取消后**仍等待远端响应或连接断开**才算该次运行结束（超时当场记失败，名额保留到真正结束），所以一个一直不返回的扩展会让对应的闹钟不再被调度，表现为健康告警，而不是被悄悄重叠执行。

## 4. 扩展 → 内核

所有 Watch 写操作都是引擎命令，走短事务。

| 方法 | 参数 | 说明 |
|---|---|---|
| `watch/receive` | `Trigger` | 写 inbox，返回 `queued\|duplicate`。**返回后推进游标是安全的** |
| `thread/describe` | `{key\|ref, label?, url?, facts?}` | 合并写 Thread 元信息；fact 值为 null 表示删除 |
| `thread/patch` | `{key, meta}` | 合并写 meta 其余键 |
| `thread/subscribed` | `{kind?}` | 只读；返回本源仍在被盯的 Thread（含保留期） |
| `thread/get` | `{key}` | 只读 |
| `work/open` | `{}` | 只读；在办 Work 摘要 |
| `work/bindThread` | `{workId, thread, own}` | 如执行方建了 PR，把它挂到 Work |
| `thread/closed` | `{thread, outcome: "done"\|"closed", at, detail?}` | 外部对象关闭 |
| `thread/reopened` | `{thread, at}` | 外部对象重新打开 |
| `state/get` `state/set` `state/keys` | — | 扩展私有 KV，**只放游标与最小差分索引，禁止存正文等完整外部对象快照** |
| `health/report` | `{entry, state: "ok"\|"paused"\|"error", detail}` | 健康上报 |
| `receipts/recent` | `{channels, filters…, afterSentSeq?, sinceMs?, limit}` | 只读；按提交时单调序号分页读已外发回执，用于消回声 |

## 5. 硬约定（协议的一致性测试检查这些）

1. **先落盘、后推进游标；重放期 < Trigger 清理期。** 必须完成当前分页组并将 Trigger 全部 `receive` 后才提交游标；所有采集与重放/恢复路径限制在 ≤7 天（远小于 30 天的 Trigger 清理期），否则过期 Trigger 被清理后重放会造成重复入队。
2. **状态事实优先于回声过滤。** 核实到最新状态后，**先** `describe` + `thread/closed|reopened`，**再**做回声过滤决定要不要发 Trigger。否则执行方自己造成的状态变化会吞掉关闭事实。
3. **禁止跨源 `Trigger.threads`**：每个 `threads[].source` 必须属于本扩展的 `watch_sources`。指向其他系统对象的引用放 `meta.references`。
4. **事件 id 确定性**：同一外部事件永远得到同一 `Trigger.id`（常用 `key(source, event, objectRef, discriminator)`），重放安全。
5. **facts 嵌套结构**：`{factId: {value, label?, problem?}}`；四个约定键见 01。
6. **ThreadRef.id 与对象标识一致**：声明了 `object_type` 的类型，`ThreadRef.id` 必须等于对象的稳定标识。
7. **错误映射**：未登录 → `health/report paused` 后正常返回；限流 → 记 `blocked_until` 后返回；可重试的不可用 → `health/report error` 并以错误返回，走闹钟退避。读到空载荷绝不当作「不存在」，只有明确的结构化「不存在」才允许关闭对象。
8. **循环里检查取消**：每个分页或每个对象检查是否已被取消。
9. **严格 JSON**：禁 `NaN/Infinity`，不依赖「宽松序列化」。

## 6. 版本与兼容

- `protocol` 是整数主版本；内核与扩展 `initialize` 时协商，不兼容则拒绝加载并报健康告警。
- 同一主版本内只加字段、加方法，不删不改语义；未知字段必须忽略。
- `pkg/protocol` 提供 Go 类型与 JSON Schema；**一致性测试套件**（`craft ext test <path>`）用录制 fixture 驱动扩展，检查 §5 的硬约定，让第三方扩展自测。

## 7. 参考实现

v1 仓库内带一个参考扩展 `extensions/github`（Go 实现，只用公开协议与公开 API），覆盖：PR 评审请求、CI 失败、PR 合并、评审评论、对外写 `pr:comment` / `pr:create`。它同时是协议的示例与一致性测试的被测对象。
