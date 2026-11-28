# 宿主适配

这里维护两个宿主（首批是 Claude Code 与 Codex）的清单、共用的 skill 与各自的 hook 文件。发布时它们和全部系统的二进制一起组成**一个通用的包**，两个宿主共用，一次安装，见 [ADR 0004](../docs/adr/0004-single-universal-package.md)。

各宿主的真实机制（清单位置、`bin/` 是否进 PATH、hook 的命令形态与信任、marketplace 来源）已在 ADR 0004 里逐项核实；其中标为「待验证」的几项，要在做宿主薄包时用真实宿主实测，结论写在这里。
