# 00 概览

状态：v1 草案。本文只含现役规则，不记录修订历史。

## 0. 框架三块

| 部分 | 回答 | 文档 |
|---|---|---|
| Watch（内核） | 做什么、谁来做、盯到什么时候 | 01、02、06、07 |
| method（方法层） | 接下的工作怎么做对 | 08 |
| outbound（对外写） | 对外的事什么时候问人 | 04 |

一条链：外部变化 → Watch 接活并派给执行方 → 执行方按 method 做好 → 对外交付经 outbound 确认。三块各自独立，只经 CLI 与标准协议对接。

## 1. 一句话

**内核只有两个互不依赖的通用部件：闹钟（到点调用）与 Watch（收事件、按订阅分发、研判、派工、跟进）。拉数据、翻译外部字段、对外系统的读写都在扩展里。**

```
扩展进程（任意语言）          内核（Go，单二进制）
 ├ 声明 alarms ─────────────► 闹钟：到点发 alarm/run，限流/退避/超时
 ├ alarm/run 里拉外部数据
 │   watch/receive ─────────► Watch：inbox → Intake → 订阅分发
 │   thread/describe                    → Thread.pending → 研判
 │                                      → Work → effect → 执行方会话
 └ 提供 action/execute ◄──── outbound：分级确认后执行对外写
```

## 1.1 术语

| 术语 | 含义 | 注意 |
|---|---|---|
| Trigger | 一条事件，由扩展把外部变化翻译而来 | 永不修改 |
| Thread | 被盯的外部对象（工单、PR、部署运行……） | 对象类型由扩展声明 |
| Work | 一件被授权去做的工作，由 Mandate 授权 | **与外部系统里的「工单」无关**；外部工单是一种 Thread |
| Mandate | 不可变的授权：谁要做、因何被准入、范围 | — |
| Assignment | Work 与一个执行实例的一次绑定 | 同一 Work 的新 Assignment 复用同一会话 |
| Extension | 进程外扩展，经 JSON-RPC 与内核对话 | — |

## 2. 原则

1. **订阅是核心。** 盯什么（Thread）、事件送给谁（订阅它的 Work）、没人认领时谁来判断（研判）。不镜像外部系统，不为展示而拉数据。
2. **内核只放机制，不放业务。** 内核里不出现任何具体系统、仓库、工单、MR 的概念；对象类型、事件类型、结束结果都是扩展声明的字符串。
3. **引擎是纯的。** 引擎同步、无 IO、时间由外部注入；一条命令一个事务；Watch 的表只有引擎写。
4. **副作用即队列。** 引擎不直接调执行方、不发消息，只在同一事务里写一行 effect；后台按 kind 执行，结果作为命令交回引擎。攒批、限流、等槽、重试、合并都由 `Thread.pending` 与 effect 表自然得到。
5. **接入面窄而稳定。** 扩展对内核只有协议里列出的方法；全部纯 JSON；扩展之间互不依赖。
6. **不阻塞。** 引擎线程不跑扩展代码、不做网络与进程调用；扩展、闹钟、宿主再慢都堵不住它。
7. **推测必须带依据。** 研判、选址等推测性输出必须带 `audit`/`evidence`，不得单独出现；推不出就说缺什么。
8. **不另存推出来的结论。** 本地库只存外部事实的缓存（可重建）、查不到的事实（人的决定、绑定）和 Watch 自己的运行记录。「当前阶段」等派生量每次现算。
9. **测试即规格。** 引擎行为用表驱动测试固定，先写用例再写实现；外部系统一律用录制 fixture，测试不联网。

## 3. 部件

| 部件 | 职责 | 不做 |
|---|---|---|
| 扩展宿主（exthost） | 读 manifest、拉起扩展子进程、维护 JSON-RPC 连接、转发闹钟与对外写 | 不调度、不判断 |
| 闹钟（alarm） | 按声明到点调用扩展；并发、超时、退避、健康、停止、立即触发 | 不知道调用来干什么，不认识 Watch |
| Watch | inbox、Intake、订阅、研判、Work、Assignment、effect、Pass | 不拉数据、不解读 `meta`、不对外写业务系统 |
| outbound | 对外写的分级、预览、确认、内容检查、回执、审计 | 不知道动作的业务含义，只认扩展声明的级别 |
| 端口实现 | Assessor（研判）、Executor（执行）、Preparer（准备目标）、Channel（回复） | — |

端口实现由扩展或宿主适配包提供，内核提供测试替身。

## 4. 进程模型

- 一个常驻 **daemon**：闹钟、扩展子进程管理、Watch 引擎线程（goroutine）、effect 执行池都在里面。
- 若干短命 **CLI 进程**：人或执行方会话调用 `craft ...`。
- **引擎是库，不是服务。** 任何进程都可以在写事务里执行一条引擎命令（例如 CLI 进程里的 `report`、`decide`）；daemon 用 SQLite 的 `PRAGMA data_version` 发现外部写入并立刻跑一轮 Pass。「单一写入方」指只有引擎代码写 Watch 的表，不是只有一个线程；SQLite 写锁负责串行。effect 只在 daemon 里执行。
- 没有 daemon 时，CLI 仍可读状态与执行引擎命令，只是不会有闹钟、研判与派工推进。

## 5. 仓库布局（目标）

```
cmd/craft/                 CLI 入口
internal/engine/          纯领域：model、derive、命令、policy、内存仓储（只许标准库）
internal/store/           SQLite 仓储与 schema（SQL 只在这里）
internal/alarm/           闹钟服务（可注入时钟）
internal/app/             运行层：inbox、effects、passes、source context
internal/exthost/         扩展进程管理与 JSON-RPC
internal/outbound/        分级确认与对外写
internal/cli/             子命令
internal/method/          方法层（只用标准库，不导入 Watch 内核任何包，见 08-method.md K1）
pkg/protocol/             公开的协议类型与 JSON Schema（扩展作者依赖它）
extensions/               参考扩展（github 等），各自独立构建
hosts/                    各宿主的薄包（skill markdown + 调用说明）
docs/
```

依赖方向只能自上而下：`cli → app → engine`；`app → store / alarm / exthost / outbound`。`internal/engine` 禁止导入除标准库以外的任何包，由 CI 守门测试检查。

## 6. 范围外（v1 不做）

- 不直接调用大模型 API。研判与执行一律借助宿主（本地 AI 编码助手）开启会话。
- 不做多用户/多租户服务端。单机单用户，状态在用户目录下的一个 SQLite 文件。
- 不做可视化面板的内核部分。面板是外部客户端，经 CLI/本地 JSON 接口读状态（见 05-cli）。
