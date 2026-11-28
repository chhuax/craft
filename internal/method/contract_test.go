package method

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var workTypes = []string{"defect", "feature", "behavior-change", "tech", "data", "ui", "cross-repo", "explore"}
var tiers = []string{"light", "standard", "heavy"}

func contract(kind, tier string) (string, string) {
	front := fmt.Sprintf("---\ncraft: 1\ndescription: 筛选记录\ntype: %s\ntier: %s\n---\n", kind, tier)
	req := front + "## 背景\n记录较多\n## 目标与范围\n筛选记录\n## 业务规则\n按目录筛选\n" + scenarios
	design := "## 结论\n保留顺序\n## 根因\n索引缺项\n## 设计\n更新索引\n## 考虑过的其他做法\n逐项扫描过慢\n## 影响范围与兼容\n旧记录可以重建索引\n" + verification
	return req, design
}

func layout(tier, req, design string) map[string]Document {
	if tier == "light" {
		return map[string]Document{"change.md": Parse("changes/unit/change.md", req+design)}
	}
	docs := map[string]Document{"requirements.md": Parse("changes/unit/requirements.md", req)}
	if design != "" {
		docs["design.md"] = Parse("changes/unit/design.md", design)
	}
	return docs
}

func TestExitRules(t *testing.T) {
	for _, kind := range workTypes {
		for _, tier := range tiers {
			req, design := contract(kind, tier)
			if gaps := CheckExit(layout(tier, req, design), false); len(gaps) != 0 {
				t.Fatalf("%s/%s: %+v", kind, tier, gaps)
			}
		}
	}
	for _, tier := range tiers {
		req, design := contract("defect", tier)
		for _, tc := range []struct{ from, to, rule, needle string }{
			{"craft: 1", "craft: true", "F1 front", "craft"}, {"craft: 1", "craft: 2", "F1 front", ""},
			{"type: defect", "type: unknown", "F1 front", "type"}, {"description: 筛选记录", "description: " + strings.Repeat("字", 101), "F1 front", "description"},
			{"craft: 1", "craft: 1\ncapabilities: 2", "F1 front", "capabilities"}, {"craft: 1", "craft: 1\nbaseBranch: []", "F1 front", "baseBranch"},
			{"记录较多", "<!-- 模板 -->", "F2 sections", "背景"}, {"## 根因", "## 其他", "F2 sections", "根因"},
			{"S-2 | 不变", "S-1 | 不变", "F3 scenarios", "S-1"}, {"S-2 | 不变", "X-1 | 不变", "F3 scenarios", "X-1"},
			{"不变:", "原先不变:", "F4 unchanged", ""}, {"记录较多", "记录较多\n## 假设与待确认\n- 待确认：含旧数据吗", "F5 open", "含旧数据吗"},
			{"S-1、S-2", "S-1", "F6 coverage", "S-2"}, {"TestFilter", "", "F6 coverage", "S-1"}, {"|自动", "|声明", "F6 coverage", "方式"},
		} {
			files := layout(tier, strings.ReplaceAll(req, tc.from, tc.to), strings.ReplaceAll(design, tc.from, tc.to))
			gaps := CheckExit(files, false)
			if !slices.ContainsFunc(gaps, func(g Gap) bool {
				return g.Rule == tc.rule && strings.Contains(g.Message, tc.needle) && strings.HasPrefix(g.Path, "changes/unit/") && g.Line >= 1
			}) {
				t.Errorf("%s %q: %+v", tier, tc.from, gaps)
			}
		}
	}
}

func TestExitLayout(t *testing.T) {
	for _, tc := range []struct {
		layoutTier, declared string
		extra                bool
	}{
		{"standard", "light", false}, {"light", "standard", false}, {"light", "heavy", false}, {"light", "light", true},
	} {
		req, design := contract("defect", tc.declared)
		files := layout(tc.layoutTier, req, design)
		if tc.extra {
			files["design.md"] = Parse("design.md", design)
		}
		if gaps := CheckExit(files, false); !slices.ContainsFunc(gaps, func(g Gap) bool { return g.Rule == "F1 front" }) {
			t.Error(tc, gaps)
		}
	}
	req, design := contract("explore", "heavy")
	design = strings.ReplaceAll(design, "场景|证据|方式", "场景|其他|方式")
	files := layout("heavy", req, design+"## 假设与待确认\n- 待确认：结论的适用范围\n")
	if gaps := CheckExit(files, false); len(gaps) != 1 || gaps[0].Rule != "F5 open" || gaps[0].Path != "changes/unit/design.md" {
		t.Error(gaps)
	}
}

func TestRequiredSections(t *testing.T) {
	for _, tc := range []struct {
		kind, tier, section string
		required            bool
	}{
		{"feature", "light", "业务规则", false}, {"tech", "standard", "业务规则", false}, {"feature", "standard", "业务规则", true},
		{"behavior-change", "standard", "影响范围与兼容", true}, {"data", "standard", "影响范围与兼容", true},
		{"tech", "standard", "影响范围与兼容", false}, {"tech", "heavy", "影响范围与兼容", true},
		{"feature", "light", "设计", false}, {"feature", "standard", "设计", true},
		{"explore", "heavy", "验收场景", false}, {"explore", "standard", "结论", true}, {"explore", "light", "目标与范围", false},
	} {
		req, design := contract(tc.kind, tc.tier)
		gaps := CheckExit(layout(tc.tier, strings.ReplaceAll(req, "## "+tc.section, "## 其他"), strings.ReplaceAll(design, "## "+tc.section, "## 其他")), false)
		if (len(gaps) > 0) != tc.required {
			t.Error(tc, gaps)
		}
	}
}

func TestDesignSkipped(t *testing.T) {
	if gaps := CheckExit(nil, true); len(gaps) != 0 {
		t.Fatal(gaps)
	}
	if gaps := CheckExit(nil, false); len(gaps) == 0 {
		t.Fatal("缺文档")
	}
	for _, tier := range tiers {
		req, design := contract("defect", tier)
		if gaps := CheckExit(layout(tier, req, ""), true); len(gaps) != 0 {
			t.Error(tier, gaps)
		}
		if gaps := CheckExit(layout(tier, req, ""), false); len(gaps) == 0 {
			t.Error(tier, "缺方案")
		}
		gaps := CheckExit(layout(tier, req+"## 假设与待确认\n- 待确认：保留哪些记录\n", strings.ReplaceAll(design, "TestFilter", "")), true)
		for _, rule := range []string{"F5 open", "F6 coverage"} {
			if !slices.ContainsFunc(gaps, func(g Gap) bool { return g.Rule == rule }) {
				t.Error(tier, gaps)
			}
		}
	}
}

func TestSkippedTemplate(t *testing.T) {
	for _, tier := range tiers {
		req, _ := contract("defect", tier)
		empty := "## 结论\n<!-- 说明 -->\n## 验证\n场景|证据|方式\n---|---|---\n"
		if gaps := CheckExit(layout(tier, req, empty), true); len(gaps) != 0 {
			t.Error(tier, gaps)
		}
	}
}

func TestSkippedVerificationText(t *testing.T) {
	req, _ := contract("defect", "standard")
	gaps := CheckExit(layout("standard", req, "## 验证\n手工检查了列表。"), true)
	if !slices.ContainsFunc(gaps, func(g Gap) bool { return g.Rule == "F6 coverage" }) {
		t.Fatal(gaps)
	}
}

func TestScaffold(t *testing.T) {
	for _, kind := range workTypes {
		for _, tier := range tiers {
			dir := filepath.Join(t.TempDir(), "unit")
			if err := Scaffold(dir, kind, tier); err != nil {
				t.Fatal(err)
			}
			files := map[string]Document{}
			names := []string{"requirements.md", "design.md"}
			if tier == "light" {
				names = []string{"change.md"}
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != len(names) {
				t.Fatal(entries, err)
			}
			for _, name := range names {
				raw, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				d := Parse(name, string(raw))
				if len(d.Issues) != 0 || d.value("craft") != 1 || strings.Contains(string(raw), "\r") {
					t.Fatal(kind, tier, name, d.Issues)
				}
				files[name] = d
			}
			main := files[names[0]]
			if main.value("type") != kind || main.value("tier") != tier {
				t.Fatal(main.Front)
			}
			if (main.Section("验收场景") != nil) != (kind != "explore") {
				t.Error(kind, tier, main.Sections)
			}
			if ds := files[names[len(names)-1]]; (ds.Section("根因") != nil) != (kind == "defect") {
				t.Error(kind, tier, ds.Sections)
			}
			req, plan := requiredSections(kind, tier)
			for _, name := range req {
				if main.Section(name) == nil {
					t.Error(kind, tier, name)
				}
			}
			design := files[names[len(names)-1]]
			for _, name := range plan {
				if design.Section(name) == nil {
					t.Error(kind, tier, name)
				}
			}
			if err := Scaffold(dir, kind, tier); err == nil {
				t.Fatal("重复创建覆盖目录")
			}
		}
	}
	for _, args := range [][2]string{{"unknown", "light"}, {"tech", "unknown"}} {
		dir := filepath.Join(t.TempDir(), "unit")
		if err := Scaffold(dir, args[0], args[1]); err == nil {
			t.Fatal(args)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestSuggestTier(t *testing.T) {
	for _, tc := range []struct {
		facts   RiskFacts
		tier    string
		reasons int
	}{
		{RiskFacts{Behaviors: 3, Rules: 6}, "light", 1}, {RiskFacts{Config: true}, "standard", 1}, {RiskFacts{Behaviors: 4}, "standard", 1},
		{RiskFacts{Interface: true}, "heavy", 1}, {RiskFacts{Schema: true}, "heavy", 1}, {RiskFacts{CrossRepo: true}, "heavy", 1}, {RiskFacts{PublicModule: true}, "heavy", 1},
		{RiskFacts{Rules: 7}, "heavy", 1}, {RiskFacts{Rules: 9}, "heavy", 1}, {RiskFacts{Config: true, Schema: true, Rules: 9}, "heavy", 3},
	} {
		tier, reasons := SuggestTier(tc.facts)
		if tier != tc.tier || len(reasons) != tc.reasons || slices.Contains(reasons, "") {
			t.Error(tc, tier, reasons)
		}
	}
}
