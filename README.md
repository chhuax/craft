# craft

> 项目名暂定，见 `docs/open-questions.md` Q1b。

craft 是一个本地优先的「事件 → 任务 → 执行」编排内核：

- 外部系统（工单、代码托管、CI、部署……）的变化被翻译成统一的**事件**（Trigger）；
- 内核按**订阅**把事件分发给在办的工作（Work），没人认领的事件进入**研判**，决定接不接；
- 接下的工作交给**执行方**（本地 AI 编码助手会话、其他 agent、人）去做，内核跟进到对象关闭；
- 任何对外写操作按**后果分级确认**，人答了才做。

内核不认识任何具体外部系统。工单、GitLab、GitHub、部署平台都是**进程外扩展**，通过 JSON-RPC 与内核对话。

## 状态

规格阶段（v1 草案）。先冻结规格，再写代码。入口：

| 文档 | 内容 |
|---|---|
| [docs/spec/00-overview.md](docs/spec/00-overview.md) | 定位、原则、部件、进程模型 |
| [docs/spec/01-data-model.md](docs/spec/01-data-model.md) | Trigger / Thread / Evaluation / Decision / Work 等 |
| [docs/spec/02-engine.md](docs/spec/02-engine.md) | 引擎命令、主流程、effect 队列、闹钟 |
| [docs/spec/03-extension-protocol.md](docs/spec/03-extension-protocol.md) | 扩展协议（JSON-RPC over stdio） |
| [docs/spec/04-confirmation.md](docs/spec/04-confirmation.md) | 对外写的分级与确认 |
| [docs/spec/05-cli.md](docs/spec/05-cli.md) | CLI 契约 |
| [docs/spec/06-runtime.md](docs/spec/06-runtime.md) | 运行层：存储连接、inbox、Pass、扩展装载与重载、保留清理 |
| [docs/spec/07-executor.md](docs/spec/07-executor.md) | 执行方与会话：launch、槽位、投递、未知结果、重启恢复、写入边界 |
| [docs/spec/08-method.md](docs/spec/08-method.md) | method 方法层：意图 → 契约 → 证据，按清晰度与风险伸缩 |
| [docs/adr/](docs/adr/) | 架构决策记录（语言、扩展形态、存储） |
| [docs/roadmap.md](docs/roadmap.md) | 里程碑、从旧项目迁移的对照与去公司化清单 |
| [docs/open-questions.md](docs/open-questions.md) | 尚未决定的问题 |

## 许可证

Apache-2.0，见 `LICENSE`。
