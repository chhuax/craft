# 宿主适配包

这里按宿主维护薄包的 manifest 模板、skill 和调用说明。首批宿主为 Claude Code 与 Codex。

按 ADR 0004，发布时将为每种「宿主 × 系统」组合生成包。构建阶段按目标系统渲染 hook 和命令入口模板，并将对应架构的 `craft` 或 `craft.exe` 装入包内。各宿主对插件根目录和 shell 的约定在实现模板时分别核实。[ADR 0004](../docs/adr/0004-per-host-per-os-packages.md)
