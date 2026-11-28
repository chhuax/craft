# craft 工作助手

在 Claude Code 或 Codex 里问「哪些工作在等我？」即可查看待办、回答后台问题或确认对外操作。两个宿主使用同一份包，包含 macOS 和 Linux 的 arm64、amd64 入口，以及 Windows amd64 入口。

## 安装与使用

1. 在所用宿主的插件市场中选择 **craft** 并启用。Claude Code 可运行 `/plugin install craft --marketplace <owner>/<repo>`，将占位符换成提供该插件的市场仓库。自建市场时，将插件来源指向已组装的分发仓库根目录，用 `url` 或 `git-subdir` 固定发布版本的 ref 或 sha。
2. 在 Codex 中审阅并信任 SessionStart hook，再打开新会话。升级后如宿主要求，请重新审阅。
3. 问「列出进行中的工作」或「有什么需要我决定」。Claude Code 里也可以运行 `craft watch status`。Codex 使用开场给出的可执行文件绝对路径。需要后台执行时，运行 `craft daemon start`。
4. 如果没有开场提示，仍可直接使用插件目录下的 `bin/craft`，Windows 使用 `bin\craft.exe`。状态存放在用户目录下的 `.craft`，也可用 `CRAFT_HOME` 指定。

## 已知限制

Claude Code 2.1.330 在 macOS arm64 上已验证 git 来源安装、可执行位和 Bash 中的裸命令。Windows 的 Claude Code hook 需要 sh。仅有 PowerShell 时请用可执行文件入口。Codex 的安装、开场上下文注入和升级后是否重新审阅，仍待真实宿主验证。
