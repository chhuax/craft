package method

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const scenarios = "## 验收场景\n编号 | 场景 | 预期\n:---|---:|---\n| S-1 | 筛选 | 含甲 \\| 乙 |\n| S-2 | 不变: 清空 | 全部 |\n"
const verification = "## 验证\n场景|证据|方式\n---|---|---\nS-1、S-2|TestFilter|自动\n\n收尾: `go test`\n"

func TestFront(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want any
	}{
		{"", nil}, {"null", nil}, {"true", true}, {"false", false}, {"-2", -2},
		{"plain text", "plain text"}, {`'it''s'`, "it's"}, {`"a: \"b\" \\ c\n\t"`, "a: \"b\" \\ c\n\t"},
		{"[]", []any{}}, {`[a, "b, c", 3]`, []any{"a", "b, c", 3}},
	} {
		d := Parse("正文.md", "\ufeff---\r\nk: "+tc.raw+"\r\n---\r\n")
		if len(d.Issues) != 0 || len(d.Front) != 1 || !reflect.DeepEqual(d.Front[0].Value, tc.want) {
			t.Errorf("%q: %+v", tc.raw, d)
		}
	}
	for _, raw := range []string{`"未闭合`, "[a, b", "[a,, b]", "[a, ]", "{x: 1}", "[[1]]", `"a" text`, `"\q"`, "|"} {
		d := Parse("a.md", "---\na: 1\nk: "+raw+"\nb: 2\n---\n")
		if len(d.Issues) != 1 || d.Issues[0].Line != 3 || d.Issues[0].Rule != "F1 front" || len(d.Front) != 2 || d.Front[1].Key != "b" {
			t.Errorf("%q: %+v", raw, d)
		}
	}
	d := Parse("a.md", "---\nz: 1\nbad\n  a: 1\nz: 2\na: 3\n---")
	if len(d.Issues) != 3 || len(d.Front) != 2 || d.Front[0].Value != 1 || d.Front[0].Key != "z" {
		t.Fatal(d)
	}
	for _, prefix := range []string{"---\ncraft: 1\n", "\n---\ncraft: 1\n---\n"} {
		d := Parse("a.md", prefix+"## 背景\n现状")
		if d.Front != nil || d.Section("背景").Content != "现状" {
			t.Fatal(d)
		}
	}
}

func TestDocument(t *testing.T) {
	text := "# 标题\r##  背景  \r<!-- 空\n## 注释里的标题\n-->\n## 设计\n```\n<!-- 保留 -->\n## 示例\n```\n前<!-- 删 -->后\n### 小节\n## 背景\n重复\n"
	d := Parse("变更说明.md", text)
	if d.LineCount != 14 || len(d.Sections) != 3 || d.Section("背景").Content != "" || d.Section("设计").Content != "```\n<!-- 保留 -->\n## 示例\n```\n前后\n### 小节" {
		t.Fatal(d.Sections)
	}
	if len(d.Issues) != 1 || d.Issues[0].Line != 13 || d.Issues[0].Path != "变更说明.md" || d.Issues[0].Rule != "F2 sections" {
		t.Fatal(d.Issues)
	}
	d = Parse("需求说明.md", scenarios+"## 假设与待确认\n- 假设：A-3 按当前目录筛选\n* 假设: 保留排序\n+ 待确认：是否含历史记录\n- 待确认：\n~~~\n- 待确认：示例\n~~~\n## 其他\n- 待确认：题外\n")
	if len(d.Scenarios) != 2 || d.Scenarios[0].Expected != "含甲 | 乙" || d.Scenarios[0].Unchanged || !d.Scenarios[1].Unchanged {
		t.Fatal(d.Scenarios)
	}
	if len(d.Assumptions) != 2 || d.Assumptions[0].ID != "A-3" || d.Assumptions[1].ID != "" || len(d.Questions) != 1 || d.Questions[0].Line != 9 {
		t.Fatal(d.Assumptions, d.Questions)
	}
	for _, sep := range []string{"、", ",", "，", " \t"} {
		d := Parse("方案说明.md", strings.ReplaceAll(verification, "、", sep))
		if len(d.Verification) != 1 || !slices.Equal(d.Verification[0].IDs, []string{"S-1", "S-2"}) || d.ClosingCommand != "go test" {
			t.Fatal(d)
		}
	}
}

func TestExplicitAssumption(t *testing.T) {
	for _, item := range []string{"- A-4：以创建时间排序", "- 假设：A-4 以创建时间排序"} {
		d := Parse("requirements.md", "## 假设与待确认\n"+item+"\n")
		if len(d.Assumptions) != 1 || d.Assumptions[0].ID != "A-4" || d.Assumptions[0].Text != "以创建时间排序" {
			t.Fatal(item, d.Assumptions)
		}
	}
}

func TestTables(t *testing.T) {
	h := "| 编号 | 场景 | 预期 |\n|---|---|---|\n"
	for _, tc := range []struct {
		name, body   string
		rows, issues int
	}{
		{"无表", "文字", -1, 0}, {"围栏", "~~~\n" + h + "~~~", -1, 0}, {"只有表头", h, 0, 0},
		{"表头错误", strings.Replace(h, "预期", "列", 1), -1, 1}, {"只取首张", h + "| S-1 |\n\n" + h, 1, 0},
	} {
		d := Parse("change.md", "## 验收场景\n"+tc.body)
		if (tc.rows < 0) != (d.Scenarios == nil) || (tc.rows >= 0 && len(d.Scenarios) != tc.rows) || len(d.Issues) != tc.issues {
			t.Error(tc.name, d)
			continue
		}
		if tc.rows == 1 && (d.Scenarios[0].Expected != "" || d.Scenarios[0].ID != "S-1") {
			t.Error(d.Scenarios)
		}
	}
	d := Parse("需求说明.md", "## 业务规则\n第一段\n续行\n\n第二段\n\n- 条目一\n  - 嵌套\n2) 条目二\n### 标题\n~~~\n示例\n~~~")
	if d.RuleCount != 4 {
		t.Fatal(d.RuleCount)
	}
}

func TestReadDocument(t *testing.T) {
	if d := ReadDocument(nil, "a.md"); len(d.Issues) != 1 || d.Issues[0].Path != "a.md" {
		t.Fatal(d)
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, data  string
		write, good bool
	}{
		{"存在.md", "\ufeff## 背景\r\n甲", true, true}, {"不存在.md", "", false, false}, {"坏编码.md", "\xff", true, false},
	} {
		if tc.write {
			if err := os.WriteFile(filepath.Join(dir, tc.name), []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
		}
		d := ReadDocument(os.DirFS(dir), tc.name)
		if tc.good {
			if len(d.Issues) > 0 || d.Section("背景").Content != "甲" {
				t.Fatal(d)
			}
		} else if len(d.Issues) != 1 || d.Issues[0].Rule != "F1 front" || d.Issues[0].Path != tc.name {
			t.Fatal(d)
		}
	}
}

// 用带注释和转义分隔符的场景及对应验证行计时，覆盖解析器实际消费的两张表。
func benchText(size int) string {
	var scenarios, verify strings.Builder
	scenarios.WriteString("## 验收场景\r\n编号|场景|预期\r\n---|---|---\r\n")
	verify.WriteString("## 验证\r\n场景|证据|方式\r\n---|---|---\r\n")
	for n := 1; scenarios.Len()+verify.Len() < size; n++ {
		id := "S-" + strconv.Itoa(n)
		scenarios.WriteString(id + "|不变：按目录筛选<!-- 示例 -->|含甲 \\| 乙\r\n")
		verify.WriteString(id + "|TestFilter|自动\r\n")
	}
	return scenarios.String() + verify.String()
}

func BenchmarkParse(b *testing.B) {
	for _, size := range []int{1 << 18, 1 << 20} {
		data := benchText(size)
		b.Run(benchmarkSize(size), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				Parse("change.md", data)
			}
		})
	}
}

func benchmarkSize(size int) string {
	if size == 1<<20 {
		return "1MiB"
	}
	return "256KiB"
}
