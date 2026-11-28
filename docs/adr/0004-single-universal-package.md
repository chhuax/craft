# ADR 0004：一个通用的胖包，两个宿主共用，一次安装

状态：提议（2026-11-28；取代 2026-11-27 的「按宿主 × 系统分别打包」）

## 背景

内核是一个 Go 二进制，要装进多个宿主（首发 Claude Code、Codex）里使用，宿主分布在 macOS、Linux、Windows 上。

2026-11-27 的初稿设想「宿主 × 系统」的包矩阵（2 × 5 = 10 个包），入口按系统在构建时渲染。2026-11-28 逐个核实宿主的真实机制后，这个前提不成立，而且用户的要求是**一键安装、只发一个包**（插件要装两次是不可接受的）。

## 核实结果（文档与旧项目的实测，不是印象）

| 事项 | Claude Code | Codex |
|---|---|---|
| 清单位置 | `.claude-plugin/plugin.json`，只有 `name` 必填；未知顶层字段被忽略并警告 | `.codex-plugin/plugin.json`（旧版兜底，仍可用）；根目录 `plugin.json` 是新的首选，两者在根文件含 `extensions.com.openai` 时不合并 |
| 组件默认位置 | `skills/`、`hooks/hooks.json`、`commands/`、`agents/`、`.mcp.json`、`bin/` | `skills/`、`hooks/hooks.json`（清单里可用 `hooks` 指向别的文件） |
| `bin/` 是否进 PATH | **是**：插件启用期间，`bin/` 里的文件在 Bash 工具的 PATH 上，模型可用裸命令调用 | 文档未提；旧项目实测**不在** PATH 上，首条命令报 127，靠 SessionStart hook 把绝对路径放进开场上下文 |
| hook 里的插件根 | `${CLAUDE_PLUGIN_ROOT}`、`${CLAUDE_PLUGIN_DATA}`（跨更新保留；插件根随更新变化，不得在其中写状态） | `PLUGIN_ROOT`、`PLUGIN_DATA`，并兼容 `CLAUDE_PLUGIN_ROOT`、`CLAUDE_PLUGIN_DATA` |
| hook 的命令形态 | shell 形式（`command` 字符串经 shell）与 **exec 形式**（`command`+`args`，不经 shell）；Windows 默认 Git Bash，没装则 PowerShell；`shell` 字段可指定 `bash`/`powershell`；exec 形式的 `command` 须解析成真实可执行文件，`.cmd`/`.bat` 不能不经 shell 直接起 | `command` 加 **`commandWindows`**（Windows 专用覆盖）、`timeout`、`statusMessage`；Windows 上由哪个 shell 执行**文档未写** |
| hook 信任 | 无逐 hook 审阅 | **首次须人工审阅信任**；信任记在 hook 的哈希上；新增或改动的 hook 被跳过直到重新信任；文档写明「插件更新改动了其中捆绑的脚本，即使 hook 定义没变也要重新审阅」；有 `--dangerously-bypass-hook-trust` |
| marketplace 来源 | 相对路径、`github`、`url`、`git-subdir`、`npm`、**`archive`（HTTPS zip + `sha256`）**、`command` | `local`、`url`（git 仓库根即插件）、`git-subdir`、`npm`；**没有 archive** |
| 缓存与版本 | 插件根随版本变化 | `~/.codex/plugins/cache/<marketplace>/<plugin>/<version>/` |
| 大小限制 | 文档未写（archive 来源有下载大小/超时限制，见其主机文档） | 文档未写 |

关键事实：
- 二进制体积不是问题：当前版本每个系统约 8 MB（`-trimpath -ldflags "-s -w"`），五个系统全放进一个包约 39 MB，压缩后约 17 MB。体积随功能增长，要在发布前复量。
- Python 版的旧项目发的是与系统无关的源码，所以天然一个包；Go 是原生二进制，要么按系统分包，要么把各系统的二进制都放进一个包并在运行时选择。

## 决定

1. **一个通用的包，两个宿主共用，包内带全部系统的二进制，一次安装。** 不再有「宿主 × 系统」矩阵，不再在构建时按系统渲染入口。
2. **同一个目录里并存两个宿主的清单**（`.claude-plugin/plugin.json` 与 `.codex-plugin/plugin.json`），互不干扰；`skills/` 共用；两个宿主的 hook 文件**分开写**（`hooks/hooks.json` 给 Claude Code，`hooks/codex-hooks.json` 给 Codex，由 Codex 清单的 `hooks` 指向），避免一个宿主不认识另一个宿主的字段。
3. **核心功能不依赖 hook。** skill 里直接调用 `craft ...`，hook 只做增强（开场上下文、执行前提醒）。这样即使某个宿主在某个系统上 hook 跑不起来，也只是降级，不是坏掉。
4. **二进制与启动器的布局**：

   ```
   bin/craft                    POSIX sh 启动器：按 uname 选二进制，exec 过去
   bin/craft.exe                Windows amd64 二进制，直接放在 bin/ 下
   bin/darwin-arm64/craft
   bin/darwin-amd64/craft
   bin/linux-amd64/craft
   bin/linux-arm64/craft
   ```

   - Claude Code：`bin/` 在 PATH 上。macOS/Linux 解析到 sh 启动器；Windows 的 PowerShell 按 PATHEXT 解析到 `craft.exe`，Git Bash 解析到 sh 启动器再转交 `craft.exe`。
   - Codex：`bin/` 不在 PATH 上。hook 用 `command`（unix，调 sh 启动器）与 `commandWindows`（调 `craft.exe`），**SessionStart 把本系统入口的绝对路径放进开场上下文**，skill 据此调用——沿用旧项目已实测验证的做法。
   - 启动器只做三件事：识别系统与架构、定位同目录下对应的二进制、`exec`。Rosetta 下 `uname -m` 报 `x86_64` 会选 amd64 二进制，可以运行，不处理。
5. **分发**：另建一个只放这个包的仓库（例如 `chhuax/craft-plugin`），发版时 CI 把整个目录提交进去并打 tag；两个宿主都用 `git-subdir` 或 `url` 来源安装它，用 `ref`/`sha` 固定版本。主仓库不放二进制，保持轻量。可执行位（755）靠 git 的文件模式保留。
   - 之后可为 Claude Code 额外提供 `archive` 来源（Releases 上的 zip + `sha256`，不用分发仓库）；Codex 没有对应来源，所以不作为主路径。
6. **版本绑定**：包版本 = 二进制版本；`craft version` 与包内 `VERSION` 不一致时拒绝工作并提示。

## 取舍

- 优点：用户只装一个东西；包只有一个；不需要按系统渲染入口；核心功能不依赖 hook，跨系统行为一致。
- 代价：
  - 每个包带约 39 MB 二进制（压缩约 17 MB），分发仓库每次发版增长约 17 MB。
  - Codex 每次升级可能重新审阅 hook（二进制在插件里，属于「捆绑的脚本」的改动——**这是对文档的推断，没有实测**）。旧项目的 Python 脚本同样在插件里，已接受这个代价。
  - macOS 经浏览器下载的二进制会被 Gatekeeper 拦；经 git 克隆（两个宿主的安装方式）不带隔离属性，不受影响。长期仍要做签名与公证。Windows 同理有 SmartScreen。

## 待验证（实现 M6/M7 时逐项实测，结论写进 `hosts/README.md`）

1. **Windows 上只有 PowerShell、没有 Git Bash** 的 Claude Code：hook 里的 `sh` 找不到，该 hook 会被跳过；核心功能（skill 调 `craft`）不受影响，但要确认 `craft.exe` 在 PowerShell 里确实按 PATHEXT 被解析到。
2. **Codex 升级是否重新审阅 hook**（改动的是二进制而非 hook 定义时）。
3. **可执行位**经 Claude Code 与 Codex 的 git 安装后是否保留；若不保留，启动器要能自举（`chmod +x` 后 exec），或改用 `sh` 显式调用。
4. Claude Code 与 Codex 的 **SessionStart 输出字段**（`hookSpecificOutput.additionalContext`）与 hook 信任流程，用真实宿主跑一遍。
5. Codex 对 `git-subdir`/`url` 来源安装这个目录布局（含 `.claude-plugin/` 并存）是否有问题。
6. 包体积随功能增长后是否触及任何宿主的限制。

`claude plugin validate` 可在本机直接校验 Claude Code 一侧的清单，是第一个要跑的检查。
