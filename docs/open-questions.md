# 待定问题

按阻塞程度排序。已定的在对应 spec / ADR 里，不在这里重复；本文只保留「已定决定的出处」与「仍未定」两部分。

## 已定（2026-11-27）

| 编号 | 决定 | 落点 |
|---|---|---|
| Q1 | **项目名定为 `craft`**（2026-11-27 用户定）。弃用 `weft`（AI 编排/agent 协调领域已有多个同名项目）与 `skein`。命令行 `craft`，状态目录 `.craft/`，环境变量 `CRAFT_HOME` | 全部文档 |
| Q2 | 「Ticket」改名 **Work**（一件被授权的工作），避免与外部工单系统混淆；Trigger、Thread、Mandate 保留 | `spec/00-overview.md` 术语表 |
| Q3 | 首发宿主支持 Claude Code 与 Codex；宿主能力矩阵放各宿主包的 manifest，内核只读 | `roadmap.md` M6 |
| Q12 | **方法论层属于框架**（2026-11-27 用户定），命名为 **method**（因整个框架已叫 craft，避免 `craft craft`）：与 Watch 并列，内核只用标准库、不导入 Watch、只经 CLI 对接（`craft method ...`）；v1 只带这一个方法论 | `spec/08-method.md` |
| Q4 | v1 研判只做规则版：`explicit` 事件直接开工，`inferred` 事件一律 `observe`（只记「本会接」不建单）；借宿主开一次性会话做研判与选址放 v1.1 | `spec/02-engine.md` §3.2 |

## 仍未定

| 编号 | 问题 | 倾向 |
|---|---|---|
| Q1b | `craft` 的辨识度风险：是常用词，GitHub 搜索会被 Craft CMS、Minecraft 类项目淹没；Homebrew formula 无占用，agent 方向无同名项目；还需查域名、商标、GitHub 组织名 | 仓库 README 与 About 里写明全称与用途，用 topic 与描述提高可搜性 |
| Q14 | 方法论层命名：`method` 是否合适，还是用别的词（例如沿用旧名「知行」的意思）；命令组 `craft method ...` 是否要缩短 | 暂用 `method` |
| Q5 | attach：研判方直接挂（可 handback）还是先给建议等人点。v1 无研判会话，attach 主要由人触发 | 直接挂 + 可 handback |
| Q6 | 汇总分析/盘点：扩展闹钟发「盘点」事件，还是人手动触发 | 两者都支持（同一事件类型） |
| Q7 | 单自有对象互斥是否进 v1 | 进，是内核通用不变式 |
| Q8 | daemon 本地 JSON 接口：回环 HTTP 还是 Unix socket / 命名管道 | 回环 HTTP + 随机端口写入状态目录，Windows 友好 |
| Q9 | 凭据保险库：自建加密文件，还是用系统钥匙串（macOS Keychain / Windows 凭据管理器 / libsecret） | 优先系统钥匙串，退化到加密文件 |
| Q10 | 多用户/团队模式是否路线图内 | 不在 v1，保留「状态目录隔离」即可 |
| Q11 | 扩展分发与签名：registry？还是纯本地目录安装 | v1 纯本地目录，不做 registry |
| Q13 | 知识沉淀（knowledge）是否进开源范围；craft 的知识源端口默认实现只读仓库内文档 | 不进 v1，作为扩展另行决定 |
| Q16 | `attention()` 的 `suggestion` 与 `health` 两类：数据不在 Work/Thread/Decision 里（健康来自 effect 层与扩展的健康上报，建议来自盘点），M3a 的纯函数只产出能从引擎状态推导的六类。做法倾向：这两类由 app 层产出同一 `AttentionItem` 类型，与引擎结果合并排序，`attention()` 保持纯函数 | M3d（effect 健康）时补合并函数；`suggestion` 随盘点放 v1.1 |
| Q17 | `Work.inputs` 没有上限：Work 是整份 JSON 文档，`Report`、`Answer` 等命令每次都要整份解码与编码（内存仓储实测目标 Work 有 500 条历史 inputs 时约 0.7 ms、3600 次分配，而只动投递窗口的 `InputsDelivered` 约 24 µs）。长寿命的 Work 会让这类命令随时间变慢。方向：对已读且已过保留期的 inputs 做裁剪或归档（可与 `prune` 一并做），或把 inputs 拆成单独集合；并给聚合大小设上限（ADR 0003 已有此约束但未落实） | M3d 之后、v0.1 之前定；先在 SQLite 上实测 5000 条 inputs 的开销再决定拆还是裁；另：SQLite 上 `InputsDelivered` 确认 10 条输入约 10 ms（`Report` 约 3 ms，`PrepareDelivery` 约 1.7 ms，内存仓储只有 0.03 ms），时间在 SQLite 写入侧，要查是不是多次写同一文档或多余的索引改写 |
| Q18 | 执行方命令（`craft watch report/handback/follow-context/mark-unrelated/read`）的**会话绑定**怎么传给 CLI：07 §11–§12 只说命令需要会话与派工，没规定传递方式。CLI 现在只接受进程入口显式注入的绑定，普通终端无绑定一律返回 4。倾向：由 launch 时的宿主适配把宿主、会话号、派工号注入环境（变量名在 M6 宿主薄包时随各宿主的注入能力一起定，不在内核里先发明）。另：05 §4 列了 `craft watch note --text T`，但 01 有 `note` 类型的 WorkInput、引擎与 07 都没有对应的写命令——要么删掉这条命令，要么定义为向 Work 追加一条 `note` 输入（谁能写、谁能读要先说清） | M6 宿主薄包前定；在此之前 `note` 返回 6 |
