# craft

craft 是一个本地优先的「事件 → 工作 → 执行」编排框架。Watch 通过闹钟和 inbox 接收变化，把工作交给执行方并跟进结果。method 帮助执行方把工作整理成可验证的变更，outbound 根据后果决定何时需要人确认对外写。

- 外部系统（工单、代码托管、CI、部署……）的变化被翻译成统一的**事件**（Trigger）；
- 内核按**订阅**把事件分发给在办的工作（Work），没人认领的事件进入**研判**，决定接不接；
- 接下的工作交给**执行方**（本地 AI 编码助手会话、其他 agent、人）去做，内核跟进到对象关闭；
- 对外写操作按后果分级。可撤回且不涉及交接的操作可自动执行，需要确认的操作先向人展示预览。

外部系统由**进程外扩展**接入，扩展经 JSON-RPC 2.0 over stdio 与内核对话。

## 状态

当前提供可编译的 CLI 骨架、时间与标识辅助函数，以及公开协议类型。CLI 支持 `craft version` 和 `craft --help`。

## 快速开始

当前从源码运行。安装 Git 和 [go.mod](go.mod) 指定的 Go 工具链后，在终端执行以下命令。

```sh
git clone https://github.com/chhuax/craft.git
cd craft
go run ./cmd/craft --help
go run ./cmd/craft version --json
```

从源码运行时，版本字段为 `dev`。最后一条命令的输出如下。

```json
{"evidence":[],"ok":true,"payload":{"version":"dev"}}
```

## 开发

使用 [go.mod](go.mod) 指定的 Go 版本，在仓库根目录运行以下命令。

```sh
make build
go run ./cmd/craft version
make test
go vet ./...
make fmt
make lint
```

`make build` 将二进制写入 `bin/`。`make lint` 使用 golangci-lint v2。开发环境与提交约定见[贡献指南](CONTRIBUTING.md)。

## 目录结构

| 目录 | 职责 |
|---|---|
| `cmd/craft/`、`internal/cli/` | 进程入口、命令解析与输出 |
| `internal/engine/`、`internal/store/` | 纯领域规则与 SQLite 仓储边界 |
| `internal/alarm/`、`internal/app/` | 闹钟与运行层，调度 inbox、Pass 和 effect |
| `internal/exthost/`、`internal/outbound/` | 管理扩展子进程与对外写确认 |
| `internal/method/` | 可独立运行的方法层 |
| `internal/boundary/` | 标准库依赖与词表守门测试 |
| `pkg/protocol/` | 扩展与内核共享的 JSON 类型 |
| `extensions/`、`hosts/` | 参考扩展与宿主薄包 |
| `tools/` | 仓库词表与将来的宿主打包工具 |
| `docs/` | 规格、架构决策与路线图 |

## 文档索引

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

craft 采用 [Apache License 2.0](LICENSE)。第三方声明见 [NOTICE](NOTICE)。
