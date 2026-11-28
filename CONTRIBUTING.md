# 贡献指南

## 准备环境

使用 [go.mod](go.mod) 指定的 Go 工具链。克隆仓库后，在根目录运行 `go build ./...` 和 `go run ./cmd/craft --help`。安装 Make 后可使用下表中的目标。

| 命令 | 用途 |
|---|---|
| `make build` | 将当前系统的 craft 编译到 `bin/` |
| `make test` | 运行单元测试与架构守门测试 |
| `make fmt` | 格式化 Go 源码 |
| `make lint` | 用 golangci-lint v2 检查代码与导入 |
| `make tidy` | 整理 Go 模块声明 |
| `make release-snapshot` | 用 GoReleaser v2 在 `dist/` 生成本地发布快照 |

## 包与协议

新增目录前，先更新[概览 §5 的布局](docs/spec/00-overview.md#5-仓库布局目标)，写明职责和依赖方向。`internal/engine` 和 `internal/method` 的生产代码都只允许导入标准库。引擎由调用方注入时间。method 应可独立运行，其他层通过 CLI 调用它。

跨进程数据使用公开协议。扩展负责解释外部系统的字段。代码、说明、注释与测试数据应使用中立术语，不得引入公司或旧项目专有词。引用外部数据前先脱敏。词表与适用范围由守门测试检查。

## 测试

用表驱动测试记录输入、预期结果与错误边界。时间用例使用固定时刻，协议用例检查 JSON 往返、可选字段和原始数值。外部服务使用脱敏的录制样本，测试须能离线运行。

提交前运行 `go build ./...`、`go vet ./...`、`go test ./...` 和 `gofmt -l .`。最后一条命令应无输出。

## 提交与评审

每个提交围绕一个可独立验证的变更。使用中文 Conventional Commits，格式为 `类型(可选范围): 中文摘要`，例如 `feat(engine): 规范化事件时间` 或 `docs: 补充扩展接入说明`。在 PR 中写明动机、可观察到的行为变化和验证结果；触及协议时附上输入输出示例。

## 发布流程

打 tag 前检查以下事项。

- [ ] 待发布提交的 CI 全绿，包括三个操作系统的构建与测试、GitHub 扩展检查和 lint。
- [ ] 确认版本号和对应的 `v` 前缀 tag 尚未使用。例如，拟发布 `v1.2.3` 时，在该提交运行 `make plugin VERSION=1.2.3`。检查构建成功，`dist/plugin/VERSION`、两份宿主清单及本机二进制版本一致。
- [ ] 查看打包报告。单个二进制超过 16 MiB、包内文件合计超过 80 MiB 都会产生体积提醒，发布前核对增长是否符合预期。[打包工具说明](tools/package/README.md)

推送以 `v` 开头的版本 tag 后，GoReleaser 将五个系统与架构组合的 CLI 归档和 `checksums.txt` 上传至该版本的 GitHub Release。流水线随后用去掉 `v` 的版本号组装通用包，将 `craft-plugin-<version>.zip`、保留可执行位的 `craft-plugin-<version>.tar.gz` 和这两个文件的 SHA-256 清单 `craft-plugin-<version>-checksums.txt` 上传到同一 Release。

要同时开放宿主安装，先准备专用于分发的 `chhuax/craft-plugin` 仓库。在**源码仓库**的 Settings → Secrets and variables → Actions 中添加名为 `PLUGIN_REPO_TOKEN` 的 repository secret。使用细粒度访问令牌，仅选择分发仓库，授予 **Contents: Read and write**。流水线将包和宿主共用的市场索引同步到分发仓库根目录，一次推送版本提交与同名 tag。未配置此 secret 时，流水线会记录跳过原因，Release 资产仍然可用。[发布工作流](.github/workflows/release.yml)
