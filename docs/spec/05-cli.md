# 05 CLI 契约

状态：v1 草案。**CLI 是唯一事实来源**：领域逻辑只在内核与扩展里；skill、面板、宿主适配只描述何时调、怎么读结果。改命令、参数、输出字段、错误码、退出码时，同一提交同步改本文。

## 1. 约定

- 二进制 `craft`，单文件，三平台（macOS、Linux、Windows）。
- 所有命令支持 `--json`；`--json` 只改输出层，不改查询与判定。成功输出统一信封：

```json
{ "ok": true, "payload": { ... }, "evidence": [ ... ] }
```

  推测性输出（推荐目标、研判结论）必须带 `evidence`，推不出就写缺什么。
- 失败输出：`{ "ok": false, "error": { "code": "...", "message": "...", "detail": { ... } } }`。
- 状态目录：默认用户目录下的 `.craft/`；环境变量 `CRAFT_HOME` 覆盖。测试一律指向临时目录，不污染真实状态。
- 凭据只经内核的凭据保险库，由认证扩展按凭据标识登记；会话文件只放状态目录。

## 2. 退出码

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 通用错误 |
| 2 | 参数错误 |
| 3 | 需要确认（`needs_confirmation`，附预览） |
| 4 | 被拒绝（硬规则或 `deny`） |
| 5 | 找不到 / 引擎拒绝（`detail.code` 为引擎错误码） |
| 6 | 依赖不可用（daemon 未运行、扩展不健康等） |

## 3. 引擎错误码

`UserError(code, values)`，`missing` 类映射到退出码 5，其余 refused 也映射到 5，`detail.code` 为下列之一：

`workEnded`、`workStopped`、`unknownTrigger`、`unknownWork`、`unknownAssignment`、`assignmentEnded`、`evalFinished`、`workerDecides`、`notAdmissible`、`notExplicit`、`notReached`、`handedBack`、`handedBackHere`、`mandateRevoked`、`notRetryable`、`unknownProblem`、`noAttention`、`threadAlreadyOwned`、`noWorkForSession`、`sourceThread`、`followedThread`、`notFollowing`、`questionWithResult`、`askerWithoutQuestion`、`notAsker`、`notAnswerable`、`ackEnded`、`ackNoChannel`、`ackNotPerson`、`notReady`、`notAttention`、`notQuestion`、`noAnswer`、`noChannel`。

编程错误（参数类型错、trigger 没有 thread）是 panic 级别的内部错误，不包成 `UserError`。

## 4. 命令组

按角色分组，角色对应 02 §2 的命令面。

```
craft daemon start|stop|status             # 常驻进程
craft doctor                                # 安装自检（宿主、扩展、状态目录）

craft watch status                          # 总览：Thread、Work、待人处理
craft watch attention                       # 待人处理清单
craft watch list [--open] [--thread K]     # 列出 Work
craft watch read <work>                   # 执行方重读 Work 当前事实
craft watch events [--since REV]            # 外发事件补拉
craft watch decisions [--kind K] [--source S] [--would-accept] [--limit N] [--cursor C]

# 人
craft watch request <thread-ref> [--text T] # 人把某对象交给系统处理
craft watch stop <work>
craft watch retry <work>
craft watch answer <work> --text T
craft watch reevaluate <trigger>
craft watch attach <work> <trigger>
craft watch unsubscribe <thread-key>
craft watch dismiss <decision>

# 执行方（只在 worker 会话里可用）
craft watch report <state> --summary T [--question Q]
craft watch handback <trigger> --reason T
craft watch note --text T
craft watch follow-context <thread-ref>
craft watch mark-unrelated <thread-key>

# 设置
craft policy show|set                       # 事件模式、放行发送人、接活规则、暂停

# 闹钟
craft alarm list
craft alarm run <ext> <alarm> [--wait [SEC]]    # 入队，默认立即返回 {queued, requestId}；--wait 轮询终态

# 对外写（见 04）
craft act <action> --params JSON [--yes] [--key K] [--target NAME]
craft outbox list|show|confirm [--yes] [--target NAME]|withdraw   # --target 是人点名要做的对象，confirm_named 级别必须与预览里的目标一致

# 扩展
craft ext list|enable|disable|logs <id>
craft ext test <path> [--fixtures DIR]     # 协议一致性测试（HTTP 重放 fixture，见 03 §5–§6）

# 凭据
craft cred list|login <provider>|logout <provider>
```

`craft alarm run ... --wait` 超时返回 `{queued:true, requestId, status, timedOut:true}`，**不取消**后台执行。

## 5. 面板与外部客户端

内核不含面板。外部客户端经下列方式读状态：

- `craft ... --json`；
- daemon 暴露的本地 JSON 接口（仅回环地址或 Unix socket / 命名管道），提供 `snapshot(sinceRev)` 与事件流，字段与 CLI 的 `--json` 一致。

面板自己的对象视图由扩展的 `views` 提供，不为单个项目改造通用面板。
