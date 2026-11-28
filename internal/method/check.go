package method

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	combinedFile     = "change.md"
	requirementsFile = "requirements.md"
	designFile       = "design.md"
)

func validType(kind string) bool {
	switch kind {
	case "defect", "feature", "behavior-change", "tech", "data", "ui", "cross-repo", "explore":
		return true
	}
	return false
}

func validTier(tier string) bool { return tier == "light" || tier == "standard" || tier == "heavy" }

// 把合一文档的需求节和方案节也分开列出，人跳过方案时仍能核对需求部分。
func requiredSections(kind, tier string) (req, design []string) {
	req = []string{"背景"}
	if tier != "light" {
		req = append(req, "目标与范围")
	}
	if kind == "explore" {
		return req, []string{"结论"}
	}
	if tier != "light" && (kind == "feature" || kind == "behavior-change") {
		req = append(req, "业务规则")
	}
	req = append(req, "验收场景")
	design = []string{"结论"}
	if kind == "defect" {
		design = append(design, "根因")
	}
	if tier != "light" {
		design = append(design, "设计", "考虑过的其他做法")
		if tier == "heavy" || kind == "behavior-change" || kind == "data" {
			design = append(design, "影响范围与兼容")
		}
	}
	return req, append(design, "验证")
}

// CheckExit 核对同一变更的文档，键为文件名，位置取自 Parse。
// designSkipped 是人明确跳过方案的事实，已写的需求、问题和验证表仍然参与核对。
func CheckExit(files map[string]Document, designSkipped bool) []Gap {
	main, combined := files[combinedFile]
	design, hasDesign := files[designFile]
	req, hasReq := files[requirementsFile]
	if !combined {
		main = req
	}
	kind, _ := main.value("type").(string)
	tier, _ := main.value("tier").(string)
	gaps := documentIssues(files, kind)
	if !combined && !hasReq {
		if hasDesign || !designSkipped {
			gaps = append(gaps, Gap{"F1 front", requirementsFile, 1, "缺少需求说明或合一文档"})
		}
		return gaps
	}
	if main.unsupported {
		return gaps
	}
	gaps = append(gaps, checkFront(main, kind, tier)...)
	if combined {
		if hasDesign || hasReq || validTier(tier) && tier != "light" {
			gaps = append(gaps, Gap{"F1 front", main.Path, 1, "合一文档与档位或其他文档冲突"})
		}
		design = main
		// 文件头与实际布局冲突时，仍按现有文件所承担的角色检查各节。
		tier = "light"
	} else {
		if tier == "light" {
			gaps = append(gaps, Gap{"F1 front", main.Path, 1, "轻档使用合一文档"})
			tier = "standard"
		}
		if !hasDesign && !designSkipped {
			gaps = append(gaps, Gap{"F1 front", path.Join(path.Dir(main.Path), designFile), 1, "缺少方案说明"})
		}
	}
	required, plan := requiredSections(kind, tier)
	gaps = append(gaps, checkSections(main, required)...)
	planPresent := (combined || hasDesign) && !design.unsupported
	if planPresent && !designSkipped {
		gaps = append(gaps, checkSections(design, plan)...)
	}
	if kind != "explore" {
		gaps = append(gaps, checkScenarios(main, kind)...)
		// 空模板可以跳过；实际写下的验证表或文字声明仍须逐个场景核对。
		written := len(design.Verification) > 0 || design.Verification == nil && design.hasText("验证")
		if planPresent && (!designSkipped || written) {
			gaps = append(gaps, checkCoverage(main, design)...)
		}
	}
	return gaps
}

func documentIssues(files map[string]Document, kind string) []Gap {
	var gaps []Gap
	for _, name := range []string{combinedFile, requirementsFile, designFile} {
		d, ok := files[name]
		if !ok {
			continue
		}
		for _, g := range d.Issues {
			if kind == "explore" && (g.Rule == "F3 scenarios" || g.Rule == "F6 coverage") {
				continue
			}
			gaps = append(gaps, g)
		}
		for _, q := range d.Questions {
			gaps = append(gaps, Gap{"F5 open", d.Path, q.Line, "待确认：" + q.Text})
		}
	}
	return gaps
}

func checkFront(d Document, kind, tier string) []Gap {
	if d.Front == nil {
		return []Gap{{"F1 front", d.Path, 1, "缺少闭合的文件头"}}
	}
	var gaps []Gap
	add := func(message string) { gaps = append(gaps, Gap{"F1 front", d.Path, 1, message}) }
	if d.value("craft") != formatVersion {
		add("craft 版本缺失或无效")
	}
	desc, ok := d.value("description").(string)
	if !ok || strings.TrimSpace(desc) == "" || utf8.RuneCountInString(desc) > 100 {
		add("description 应为 1 至 100 字的说明")
	}
	if !validType(kind) {
		add("type 缺失或未知")
	}
	if !validTier(tier) {
		add("tier 缺失或未知")
	}
	for _, key := range []string{"capabilities", "workItems"} {
		v := d.value(key)
		if _, ok := v.([]any); v != nil && !ok {
			add(key + " 不是列表")
		}
	}
	v := d.value("baseBranch")
	if _, ok := v.(string); v != nil && !ok {
		add("baseBranch 不是字符串")
	}
	return gaps
}

func checkSections(d Document, names []string) []Gap {
	var gaps []Gap
	for _, name := range names {
		s := d.Section(name)
		switch {
		case s == nil:
			gaps = append(gaps, Gap{"F2 sections", d.Path, 1, "节「" + name + "」缺失"})
		case s.Content == "":
			gaps = append(gaps, Gap{"F2 sections", d.Path, s.Line, "节「" + name + "」为空"})
		}
	}
	return gaps
}

var scenarioID = regexp.MustCompile(`^S-[0-9]+$`)

func checkScenarios(d Document, kind string) []Gap {
	var gaps []Gap
	s := d.Section("验收场景")
	line := 1
	if s != nil {
		line = s.Line
		badHeader := slices.ContainsFunc(d.Issues, func(g Gap) bool { return g.Rule == "F3 scenarios" })
		if s.Content != "" && len(d.Scenarios) == 0 && !badHeader {
			gaps = append(gaps, Gap{"F3 scenarios", d.Path, line, "验收场景缺少表格或数据行"})
		}
	}
	seen := make(map[string]bool)
	var unchanged bool
	for _, row := range d.Scenarios {
		if !scenarioID.MatchString(row.ID) {
			gaps = append(gaps, Gap{"F3 scenarios", d.Path, row.Line, fmt.Sprintf("场景编号格式有误：%q", row.ID)})
		}
		if seen[row.ID] {
			gaps = append(gaps, Gap{"F3 scenarios", d.Path, row.Line, "场景编号重复：" + row.ID})
		}
		seen[row.ID] = true
		unchanged = unchanged || row.Unchanged
	}
	if !unchanged && (kind == "defect" || kind == "behavior-change" || kind == "tech") {
		gaps = append(gaps, Gap{"F4 unchanged", d.Path, line, "缺少标为「不变」的验收场景"})
	}
	return gaps
}

func checkCoverage(req, design Document) []Gap {
	if len(req.Scenarios) == 0 {
		return nil
	}
	var gaps []Gap
	covered := make(map[string]bool)
	for _, row := range design.Verification {
		if row.Method != "自动" && row.Method != "手工" {
			gaps = append(gaps, Gap{"F6 coverage", design.Path, row.Line, "未识别的验证方式：" + row.Method})
		}
		if row.Evidence != "" {
			for _, id := range row.IDs {
				covered[id] = true
			}
		}
	}
	var missing []string
	for _, s := range req.Scenarios {
		if s.ID != "" && !covered[s.ID] {
			missing = append(missing, s.ID)
			covered[s.ID] = true
		}
	}
	if len(missing) > 0 {
		line := 1
		if s := design.Section("验证"); s != nil {
			line = s.Line
		}
		gaps = append(gaps, Gap{"F6 coverage", design.Path, line, "验证表中缺少证据：" + strings.Join(missing, "、")})
	}
	return gaps
}
