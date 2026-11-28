package method

import (
	"fmt"
	"strings"
)

func sectionText(d Document, name string) string {
	if s := d.Section(name); s != nil {
		return s.Content
	}
	return ""
}

// ProjectChange 从契约及证据生成变更说明。保留原文 Markdown，缺项显式留空，守护测试按场景列出。
func ProjectChange(files map[string]Document, log []Record, current string) string {
	req, design := files[requirementsFile], files[designFile]
	if combined, ok := files[combinedFile]; ok {
		req, design = combined, combined
	}
	var out strings.Builder
	section := func(title, body string) {
		if body == "" {
			body = "<未填写：" + title + ">"
		}
		fmt.Fprintf(&out, "## %s\n\n%s\n\n", title, body)
	}
	section("问题", sectionText(req, "背景"))
	if req.value("type") == "defect" {
		section("根因", sectionText(design, "根因"))
	}
	section("做法", sectionText(design, "结论"))
	scope := sectionText(req, "目标与范围")
	for _, title := range []string{"非目标", "不做什么"} {
		if value := sectionText(req, title); value != "" {
			scope = strings.TrimSpace(scope + "\n\n" + value)
		}
	}
	section("范围与非目标", scope)
	for _, pair := range [][2]string{{"考虑过的其他做法", "没采用"}, {"影响范围与兼容", "风险与回退"}, {"接口与库表变更", "接口与库表变更"}} {
		if body := sectionText(design, pair[0]); body != "" {
			section(pair[1], body)
		}
	}
	section("守护测试", guardTests(req, design))
	var assumptions []string
	for _, a := range req.Assumptions {
		assumptions = append(assumptions, strings.TrimSpace("- "+a.ID+" "+a.Text))
	}
	if len(assumptions) == 0 {
		assumptions = []string{"无已登记假设"}
	}
	section("待人确认", strings.Join(assumptions, "\n"))
	section("验证结论", verificationText(req, log, current))
	return strings.TrimSuffix(out.String(), "\n")
}

func guardTests(req, design Document) string {
	guards := map[string][]string{}
	seen := map[[3]string]bool{}
	for _, r := range design.Verification {
		if r.Evidence == "" {
			continue
		}
		for _, id := range r.IDs {
			key := [3]string{id, r.Method, r.Evidence}
			if !seen[key] {
				guards[id] = append(guards[id], r.Method+"："+r.Evidence)
				seen[key] = true
			}
		}
	}
	var rows []string
	for _, s := range req.Scenarios {
		entries := guards[s.ID]
		if len(entries) == 0 {
			entries = []string{"<未填写：验证依据>"}
		}
		rows = append(rows, "- "+s.ID+" "+s.Text+" — "+strings.Join(entries, "；"))
	}
	return strings.Join(rows, "\n")
}

func verdictText(state string) string {
	switch state {
	case "pass":
		return "通过"
	case "fail":
		return "失败"
	case "stale":
		return "已过期"
	case "unverified":
		return "无法验证"
	default:
		return "未验证"
	}
}

func verificationText(req Document, log []Record, current string) string {
	status := AssessEvidence(req, log, current)
	full, review := latestEvidence(log)
	var out strings.Builder
	fmt.Fprintf(&out, "- 收尾验证：%s", verdictText(status.Closing))
	if full >= 0 {
		fmt.Fprintf(&out, "（%s）", log[full].Closing.Command)
	}
	fmt.Fprintf(&out, "\n- 独立验证：%s", verdictText(status.Review))
	if review >= 0 && status.Review != "stale" {
		r := log[review].Review.Report
		fmt.Fprintf(&out, "（%s）", r.By)
		for _, s := range r.Scenarios {
			fmt.Fprintf(&out, "\n  - %s %s — %s", s.ID, verdictText(s.Result), s.Evidence)
		}
	}
	return out.String()
}
