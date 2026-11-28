---
name: method
description: 接到模糊的需求或缺陷，需要弄清意图、写需求说明与方案说明、按任务实现，或要做验证和交付说明时使用。
---

# 从需求到可核对的变更

Claude Code 的 Bash 可以直接运行下列命令。Codex 用开场提示的可执行文件绝对路径替换示例里的 `craft`。没有提示时，从本 skill 目录向上两级到插件根，使用 `bin/craft` 或 Windows 的 `bin\craft.exe`。路径含空格时加引号，PowerShell 用 `&` 调用。命令默认使用当前项目，可带 `--project DIR`、`--json`。

1. **还原意图。** 先从代码和实际例子查明现状。清楚的需求用一段话回放目标和范围。有多种理解时，问一个能区分它们的例子。无人回答时留下候选解释与问题，人同意暂按某种解释做时记为假设。 [意图还原](references/intent.md)
2. **准备契约。** 按主类型和风险定档。例如 `craft method new --name filter-names --type feature --tier standard --base-branch main --draft`。在返回的文件里写需求和方案，轻档两份合一。用 `craft method status --list` 查变更，多份变更时带 `--change ID`。 [工作类型](references/types.md) · [分档](references/tiers.md) · [写法](references/writing.md)
3. **让人确认。** 用 `craft method check --for confirm` 核对缺口，把预期行为、关键例子、方案取舍、风险和假设直接给人看。重档在确认前独立评审。获得认可后用 `craft method confirm --by "确认人"` 记下本次文档摘要。正文变动后重新确认。
4. **实现。** 标准档、重档按验收场景拆任务，完成窄验证后按任务本地提交。中断后用 `craft method tasks` 从提交恢复进度。 [任务卡](references/tasks.md) · [实现](references/implement.md)
5. **验证与交付说明。** 收尾验证绑定代码快照，独立验证逐条给出场景结论。用 `craft method evidence` 查有效性，`craft method check --for exit` 核对文档出口。存在接口或库表增量时合并现状，用 `craft method note` 起草变更说明，把未验证项与待人确认的假设一起交给读者。 [验证](references/verify.md) · [接口与库表](references/interfaces.md)

人明确要求「直接改」「不用写方案」时，使用 `craft method fact --change quick-fix --text design-skipped`，把短名换成本次变更。直接实现，说明采用了哪种理解、实测结果和剩余疑问。已有需求中的待确认问题仍须处理。用 `craft method flow` 查看当前文档进度。

示例按档位选读。 [轻档缺陷](examples/light-defect/change.md) · [标准档需求](examples/standard-feature/requirements.md) · [配套方案](examples/standard-feature/design.md) · [配套任务](examples/standard-feature/tasks.md)
