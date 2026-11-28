# 参考扩展

这里放独立构建的参考扩展。每个扩展有自己的目录、manifest 和启动命令。扩展在子进程中运行，通过标准输入输出上的 JSON-RPC 2.0 接收内核请求、提交事件和对象事实。Go 扩展可复用 `pkg/protocol` 中的公开类型。

事件翻译、来源游标和具体系统的读写由对应扩展负责。[扩展协议](../docs/spec/03-extension-protocol.md)

## GitHub

`make build` 在 `extensions/github/` 生成可执行文件。Windows 的可装载目录是 `bin/extensions/github/`，其中的 manifest 使用 `.exe` 文件名。把可执行文件和 manifest 放在同一个名为 `github` 的扩展目录即可装载。

扩展从 `initialize.config` 读取 `apiBase`、`tokenEnv` 和 `pollWindowDays`。默认连接 `https://api.github.com`，从进程环境变量 `GITHUB_TOKEN` 读取令牌，首次回看 7 天。回看天数可以设为 1–7。当前 daemon 传入空配置，使用上述默认值。

| 动作 | `--params` 参数 |
|---|---|
| `pr:get` | `repo`（如 `north/demo`）、`number` |
| `pr:comment` | `repo`、`number`、`body` |
| `pr:create` | `repo`、`head`、`base`、`title`，可选 `body`。执行方在工作中创建 PR 时传 `workId`，创建后关联为跟随对象 |
| `pr:merge` | `repo`、`number`。确认执行时用 `--target 'north/demo#5'` 点名预览中的对象 |

运行 `go test ./internal/conformance -run '^TestGitHubExtension$'` 可在临时目录用正式扩展和本地 HTTP 重放核对协议。手动验收时先运行 `make build`，将 `CRAFT_FIXTURE_GITHUB_TOKEN` 设为离线占位值 `offline-replay`，执行 `bin/craft ext test extensions/github`。Windows 使用 `bin/craft.exe ext test bin/extensions/github --fixtures extensions/github/testdata/conformance`。
