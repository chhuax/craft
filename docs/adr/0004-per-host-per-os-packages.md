# ADR 0004：按宿主 × 系统分别打包，入口按系统生成

状态：提议（2026-11-27，用户定方向）

## 背景

内核是一个 Go 二进制，要装进多个宿主（首发 Claude Code、Codex）里使用，宿主分布在 macOS、Linux、Windows 上。宿主插件的 hook 与命令入口在不同系统上写法不同（路径分隔符、可执行文件后缀、shell 差异）。

## 决定

1. **每次发布产出「宿主 × 系统」的包矩阵**，每个包只含本宿主、本系统所需的内容，**包内不做平台选择，也不在运行时下载**。
2. **包 = 宿主薄壳 + 本系统的二进制 + 本系统的入口**。
3. **入口按系统在构建时生成**：宿主 manifest 里的 hook、命令的 `command` 字段由模板按系统渲染，不手写、不在运行时判断系统。
4. 版本绑定：包版本 = 二进制版本；`craft version` 自检，与包内 `VERSION` 不一致时拒绝工作并提示。

## 矩阵与命名

| 维度 | 取值 |
|---|---|
| 宿主 | `claude-code`、`codex`（后续增加宿主只加一行） |
| 系统/架构 | `darwin-arm64`、`darwin-amd64`、`linux-amd64`、`linux-arm64`、`windows-amd64` |

首发 2 × 5 = 10 个包。文件名：`craft-<host>-<version>-<os>-<arch>.tar.gz`（Windows 用 `.zip`），同时发布 `checksums.txt`。

## 包布局

```
craft-<host>-<version>-<os>-<arch>/
  <宿主要求的 manifest 与目录>       # 由 hosts/<host>/ 的模板渲染，hook 入口已是本系统写法
  skills/ ...                       # 宿主薄包里的 skill 与说明（来自 hosts/_shared 与 hosts/<host>）
  bin/craft  或  bin/craft.exe      # 本系统二进制
  VERSION
  LICENSE  NOTICE
```

构建脚本 `tools/package/` 负责：编译二进制 → 渲染宿主模板 → 归档 → 生成校验和。入口模板按系统分支：

- unix 系：`"${PLUGIN_ROOT}/bin/craft" <子命令>`；
- windows：`"%PLUGIN_ROOT%\bin\craft.exe" <子命令>`。

各宿主引用「插件根目录」的占位符、Windows 上 hook 由哪个 shell 执行，**各宿主不同，实现前逐个核实**，写进 `hosts/<host>/README.md`，不要凭印象写。

## 上传

- **产物上传到 GitHub Releases**（附 `checksums.txt`，将来可加签名）。二进制与归档**不提交进主仓库的 `main` 历史**，避免仓库膨胀。
- 如果宿主的插件市场要求「git 仓库」作为安装源，另建**只读的分发仓库或分发分支**，由 CI 把每个包渲染好的内容推上去（每个「宿主 × 系统」一个目录或一个 tag）。这个需求取决于宿主的安装机制，待 M6 时核实后定。

## 理由

- 用户装哪个包就得到哪套入口，**没有运行时判断，也没有首次联网下载**，离线与受限网络可用。
- 平台差异集中在构建模板里，一处生成，不散落在各 hook 脚本里。
- 与旧项目「包 = 公共运行时 + 本宿主目录，原样成为宿主包根」的做法一致；Go 让公共运行时缩成一个二进制，包体更小。

## 代价与待核实

- 包数量随「宿主 × 系统」相乘增长，发布产物多；用 CI 矩阵与 goreleaser 的自定义归档解决。
- 每个包带完整二进制（约 10–20 MB），升级是整包替换。
- macOS 二进制未签名公证时，经浏览器下载会被 Gatekeeper 拦；经 brew 或命令行下载不受影响。长期要做签名与公证。Windows 同理有 SmartScreen。
- 宿主对插件包大小、可执行文件、hook 命令的限制，逐个核实。
