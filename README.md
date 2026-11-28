# craft

[English](README.en.md)

craft 是一个本地优先的工作编排工具。Watch 把外部系统的变化记为 Trigger，按所跟进的对象（Thread）找到已有的 Work，或研判是否接下新工作。接下后，craft 将 Work 交给执行方，跟进进展，并把问题、待审结果和需要确认的对外操作交给人。

## 当前能力

- **Watch 与 daemon**。daemon 调度闹钟、处理 inbox、派工并投递后续事件。事件和工作状态保存在本地 SQLite 中，人可以查看在办 Work、回答问题或重试派工。
- **GitHub 参考扩展**。独立的扩展进程经标准输入输出用 JSON-RPC 2.0 与内核通信。它轮询 GitHub 通知和已订阅对象，可接收 PR 评审请求、议题指派等 Trigger，也提供 PR 查询、评论、创建与合并动作。
- **Claude Code 与 Codex**。两个宿主共用一组 skill。在主会话里可以问「哪些工作在等我？」，后台执行方则读取自己的 Work、回报进展或向人提问。
- **method**。在 Git 项目中记录变更意图和验收场景，按文档内容记录确认，从提交恢复任务进度，把验证证据绑定到代码快照。
- **outbound 与凭据**。对外动作按后果分为自动执行、预览后确认、点名目标后确认和拒绝。凭据优先保存在系统钥匙串，不可用时回退到本地加密文件。扩展启动时只注入它声明的凭据。

## 安装

**首个版本发布后可用。** 分发仓库 `chhuax/craft-plugin` 尚未创建。按你使用的宿主选一行，在终端安装同一个通用包。包内含 macOS 和 Linux 的 arm64、amd64 二进制，以及 Windows amd64 二进制。

**Claude Code**

```sh
claude plugin install craft --marketplace chhuax/craft-plugin
```

**Codex**

```sh
codex plugin marketplace add chhuax/craft-plugin && codex plugin add craft@craft-plugin
```

Claude Code 的 Bash 工具可直接调用 `craft`。在 Codex 中审阅并信任 hook，再打开新会话。如果开场提示给出了可执行文件绝对路径，就用它替换示例中的 `craft`。在普通终端或没有开场提示时，可直接使用插件目录中的 `bin/craft`，Windows 使用 `bin/craft.exe`。Codex 真实会话中的开场提示和 Windows 上的 Codex hook 尚未验证。二进制签名与公证仍在计划中。[宿主说明](hosts/README.md) · [通用包决策与验证记录](docs/adr/0004-single-universal-package.md)

## 五分钟上手

首次使用时，用 `craft doctor` 检查状态目录、数据库和已发现的扩展，然后启动 daemon。要试用 GitHub，请在启动前按下方说明准备扩展。

```sh
craft doctor
craft daemon start
craft watch status
craft watch list --open
craft watch attention
```

后三条命令分别查看总览、进行中的 Work 和需要你处理的事项。新状态目录里可能还没有 Work。默认状态目录为 `~/.craft`，可用 `CRAFT_HOME` 指定其他位置。

**接入 GitHub（可选）**。参考扩展随源码单独构建。在源码仓库运行 `make build`，将 `extensions/github/` 中的 `manifest.json` 和 `craft-ext-github` 一起复制到状态目录下的 `extensions/github/`。Windows 从 `bin/extensions/github/` 复制生成的清单和 `craft-ext-github.exe`。daemon 启动并发现扩展后，在自己的终端运行以下命令。登录命令从标准输入读取一行 GitHub 令牌，重启 daemon 后会载入凭据并应用启用设置。[参考扩展说明](extensions/README.md)

```sh
craft cred login github
craft ext enable github
craft daemon stop
craft daemon start
craft ext list
```

## 开发与目录结构

需要 Git、Make 和 [go.mod](go.mod) 指定的 Go 版本。在源码仓库根目录运行以下命令即可构建本机 CLI 和 GitHub 扩展、查看命令帮助并测试两个 Go 模块。首版发布前也可直接使用构建出的 `bin/craft`（Windows 为 `bin/craft.exe`）体验上述流程。

```sh
make build
go run ./cmd/craft --help
make test
```

| 路径 | 用途 |
|---|---|
| `cmd/craft/`、`internal/cli/` | CLI 入口、命令与输出 |
| `internal/engine/`、`internal/app/`、`internal/alarm/`、`internal/store/` | Watch 规则、daemon、调度和存储 |
| `pkg/protocol/`、`internal/exthost/`、`extensions/github/` | 扩展协议、进程管理与参考实现 |
| `internal/host/`、`hosts/` | 宿主驱动、清单、hook 和共用 skill |
| `internal/method/`、`internal/outbound/`、`internal/cred/` | 变更方法、对外确认与凭据保管 |
| `tools/package/`、`internal/boundary/` | 通用包组装与校验、架构与词表测试 |
| `docs/spec/`、`docs/adr/` | 行为规格与架构决策记录 |

从[规格概览](docs/spec/00-overview.md)进入各部件的设计，命令与参数见 [CLI 契约](docs/spec/05-cli.md)。打包和提交约定见[打包说明](tools/package/README.md)与[贡献指南](CONTRIBUTING.md)。

## 许可证

craft 采用 [Apache License 2.0](LICENSE)。第三方声明见 [NOTICE](NOTICE)。
