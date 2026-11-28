package method

import (
	"reflect"
	"strings"
	"testing"
)

func TestChangeProjection(t *testing.T) {
	req, design := contract("defect", "standard")
	req = strings.Replace(req, "筛选记录\n## 业务规则", "只动列表。\n\n不做导出。\n## 业务规则", 1) + "## 假设与待确认\n- A-1：保留顺序\n"
	design = strings.Replace(design, "索引缺项", "reader.go:7 缺少索引\n\n导致遗漏。", 1)
	for _, tier := range []string{"light", "standard"} {
		files := layout(tier, req, design)
		text := ProjectChange(files, nil, "")
		for _, piece := range []string{"记录较多", "reader.go:7 缺少索引\n\n导致遗漏。", "保留顺序", "不做导出", "S-1", "S-2", "TestFilter", "A-1", "待人确认", "未验证"} {
			if !strings.Contains(text, piece) {
				t.Errorf("%s 缺少 %s：%s", tier, piece, text)
			}
		}
		if !strings.HasSuffix(text, "\n") || strings.Contains(text, "## 验证结论\n\n通过") {
			t.Fatal(text)
		}
	}
}

func TestMergeInterfaces(t *testing.T) {
	api := InterfaceFile{"目录", "api"}
	table := InterfaceFile{"目录", "table"}
	source := "# 接口\r\n\r\n## GET /old\r\n\r\n原说明\r\n\r\n## GET /keep\r\n\r\n保留。\r\n"
	base := map[InterfaceFile]string{api: source, table: "# 表\n\n## old_table\n\n待删。\n"}
	section := "## 接口与库表变更\n### 目录\n#### 修改 GET /old\n新说明\n\n~~~\n## 示例\n#### 新增 GET /ignored\n~~~\n#### 新增 POST /new\n创建。\n#### 修改 new_table\n字段。\n#### 删除 old_table\n#### 删除 absent\n#### 配置 size\n大小\n"
	doc := Parse("design.md", section)
	merged, err := MergeInterfaces(base, doc, "change")
	if err != nil {
		t.Fatal(err)
	}
	if base[api] != source || len(merged) != 2 || !strings.Contains(merged[api], "## GET /keep\n\n保留。") || !strings.Contains(merged[api], "新说明") || strings.Contains(merged[api], "原说明") || !strings.Contains(merged[table], "## new_table") || strings.Contains(merged[table], "old_table") || strings.Contains(merged[table], "size") || strings.Contains(merged[api], "\r") {
		t.Fatal(merged)
	}
	again, err := MergeInterfaces(merged, doc, "change")
	if err != nil || !reflect.DeepEqual(again, merged) {
		t.Fatal(again, err)
	}
	for _, bad := range []string{strings.Replace(section, "修改 GET /old", "新增 GET /old", 1), section + "#### 新增 POST /new\n", section + "#### 新增 GET bad\n", strings.Replace(section, "### 目录", "", 1)} {
		got, err := MergeInterfaces(base, Parse("design.md", bad), "change")
		if err == nil || !reflect.DeepEqual(got, base) {
			t.Fatal(got, err)
		}
	}
}

func TestMergeConservesText(t *testing.T) {
	file := InterfaceFile{"目录", "api"}
	for _, tc := range []struct {
		base, delta string
		conflict    bool
	}{
		{"# 文档\r\n\r\n## GET /keep\r\n保留空白  \r\n", "删除 GET /absent", false},
		{"## GET /same\n原文\n## GET   /same\n另一段\n", "修改 GET /same", true},
		{"## GET /same\n\n人工补充\n\n> 最近变更：change\n", "新增 GET /same", true},
		{"## GET /same\n\n~~~\n", "修改 GET /same", true},
	} {
		base := map[InterfaceFile]string{file: tc.base}
		plan := Parse("design.md", "## 接口与库表变更\n### 目录\n#### "+tc.delta+"\n")
		got, err := MergeInterfaces(base, plan, "change")
		if (err != nil) != tc.conflict || !reflect.DeepEqual(got, base) {
			t.Error(tc, got, err)
		}
	}
}

func TestCommittedChangeProjection(t *testing.T) {
	dir := testRepo(t)
	req := reportDoc()
	store, err := NewFileStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	design := Parse("design.md", verification)
	c, err := Confirm("确认人", instant, req, design)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("change", *c); err != nil {
		t.Fatal(err)
	}
	taskText := card("read", "无", "base")
	if err := store.SaveTasks("change", taskText); err != nil {
		t.Fatal(err)
	}
	tasks, err := ParseTasks(taskText, req.Scenarios)
	if err != nil {
		t.Fatal(err)
	}
	repoGit(t, dir, "commit", "--allow-empty", "-qm", "读取记录\n\nTask: change/read")
	base, err := BaseCommit(t.Context(), dir, "main")
	if err != nil {
		t.Fatal(err)
	}
	commits, err := Commits(t.Context(), dir, base)
	if err != nil {
		t.Fatal(err)
	}
	if p := Continuation("change", tasks, commits); len(p.Remaining) != 0 || len(p.Done) != 1 {
		t.Fatal(p)
	}
	snap := takeSnapshot(t, dir)
	zero := 0
	if err := store.RecordClosing("change", instant, ClosingRun{Command: "go test ./...", Exit: &zero, Before: snap, After: snap}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordReview("change", instant, snap, req, reviewer()); err != nil {
		t.Fatal(err)
	}
	log, err := store.LoadLedger("change")
	if err != nil {
		t.Fatal(err)
	}
	repoFile(t, dir, "base", "新改动")
	another := takeSnapshot(t, dir)
	files := map[string]Document{requirementsFile: req, designFile: design}
	for _, tc := range []struct {
		digest, label string
		showReport    bool
	}{{snap, "通过", true}, {another, "已过期", false}, {"", "已过期", false}} {
		body := ProjectChange(files, log, tc.digest)
		if !strings.Contains(body, "独立验证："+tc.label) || strings.Contains(body, "S-1 通过 — TestRead") != tc.showReport {
			t.Fatal(body)
		}
	}
}
