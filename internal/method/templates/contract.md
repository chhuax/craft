---
craft: {{.Version}}
{{if .Main -}}
description: ""
type: {{.Kind}}
tier: {{.Tier}}
workItems: []
baseBranch: null
capabilities: []
{{end -}}
---

# （{{.Title}}）
{{range .Sections}}
## {{.}}
{{if eq . "背景" -}}
<!-- 写明遇到的问题、当前行为和出处。{{if eq $.Kind "defect"}}列出现象、期望和期望的出处。{{else if eq $.Kind "explore"}}列出本次探索要回答的问题。{{end}} -->
{{else if eq . "目标与范围" -}}
<!-- 写明目标、边界和本次不做的事。 -->
{{else if eq . "业务规则" -}}
<!-- 每条规则附一个具体例子，包含反例和边界。 -->
{{else if eq . "验收场景" -}}
<!-- 每行一个 S-n。{{if eq $.Kind "data"}}写明执行前后状态、重复执行结果与回退。{{else if eq $.Kind "ui"}}写明页面、操作、可见结果。{{else if eq $.Kind "cross-repo"}}以同一接口契约核对提供方和调用方。{{else if eq $.Kind "tech"}}列出以「不变：」开头的行为、指标与基线。{{else if eq $.Kind "behavior-change" "defect"}}列出变更前后行为及以「不变：」开头的守护场景。{{else}}列出正例、反例和边界。{{end}} -->
| 编号 | 场景 | 预期 |
|---|---|---|
{{else if eq . "假设与待确认" -}}
<!-- - 假设：A-1 内容（同意人及核对时机）；- 待确认：尚未回答的问题。 -->
{{else if eq . "根因" -}}
<!-- 列出代码位置、因果链及复现情况，区分已知事实与推断。 -->
{{else if eq . "结论" -}}
<!-- {{if eq $.Kind "explore"}}逐项回答探索的问题，给出结论的依据。{{else}}概括做法、理由和风险。{{end}} -->
{{else if eq . "设计" -}}
<!-- 说明数据流、错误处理以及各项决定的理由。 -->
{{else if eq . "考虑过的其他做法" -}}
<!-- 记录考虑过的做法及取舍依据。 -->
{{else if eq . "影响范围与兼容" -}}
<!-- 列出调用链或检索结果、旧数据和旧调用方的兼容方式，以及回退办法。 -->
{{else if eq . "接口与库表变更" -}}
<!-- 有增量时记录受影响的接口与库表。 -->
{{else if eq . "验证" -}}
<!-- 每行对应一个或多个 S-n，方式填「自动」或「手工」。表后可写收尾命令。 -->
| 场景 | 证据 | 方式 |
|---|---|---|
{{end -}}
{{end -}}
