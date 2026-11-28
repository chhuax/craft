---
craft: 1
---
## filter-prefix

- 依赖：无
- 目标文件：`catalog/filter.go`、`catalog/filter_test.go`
- 对应：S-1
- 说明：从有序目录收集名称前缀匹配项。
- 接口：`Filter(names []string, prefix string) []string`
- 验证：`go test ./catalog -run TestFilterPrefix` 覆盖正例及非前缀反例。

## restore-list

- 依赖：filter-prefix
- 目标文件：`catalog/view.go`、`catalog/view_test.go`
- 对应：S-2
- 说明：清空输入时展示原列表。
- 接口：`View.SetPrefix(prefix string)`
- 验证：`go test ./catalog -run TestClearFilter` 核对清空后的三项与顺序。
