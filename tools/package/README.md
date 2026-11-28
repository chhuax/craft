# 插件包构建

本目录将在宿主适配就绪后承担下面几项工作。包只有一个，两个宿主共用，布局与取舍见 [ADR 0004](../../docs/adr/0004-single-universal-package.md)。

1. 按目标系统和架构编译 CLI（`darwin-arm64`、`darwin-amd64`、`linux-amd64`、`linux-arm64`、`windows-amd64`），注入发布版本，用 `-trimpath -ldflags "-s -w"`。
2. 把二进制放进包的 `bin/`：Windows 的放 `bin/craft.exe`，其余放 `bin/<系统>-<架构>/craft`；放入 POSIX sh 启动器 `bin/craft`，保证可执行位。
3. 从 `hosts/` 复制两个宿主的清单、共用的 `skills/` 与各自的 hook 文件，写入 `VERSION`、`LICENSE`、`NOTICE`。
4. 校验：`claude plugin validate`、清单与 hook 路径存在性、`craft version` 与 `VERSION` 一致、可执行位。
5. 把整个目录提交到分发仓库并打 tag（见 ADR 0004 的「分发」一节）。

**不做**按系统渲染入口，也不产出「宿主 × 系统」的归档矩阵。
