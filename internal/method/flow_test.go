package method

import "testing"

func TestFlow(t *testing.T) {
	req, design := contract("defect", "standard")
	files := layout("standard", req, design)
	c, err := Confirm("人", instant, files["requirements.md"], files["design.md"])
	if err != nil {
		t.Fatal(err)
	}
	state := func(stages []Stage, name string) string {
		for _, s := range stages {
			if s.Name == name {
				return s.State
			}
		}
		return ""
	}
	for _, tc := range []struct {
		kind       string
		files      map[string]Document
		skip       bool
		name, want string
	}{
		{"defect", files, false, "根因", "done"}, {"feature", files, false, "根因", ""}, {"explore", nil, false, "验收场景", ""},
		{"feature", nil, false, "业务规则", "pending"}, {"defect", files, false, "确认", "done"},
		{"defect", layout("standard", req, design+"补充"), false, "确认", "stale"}, {"defect", layout("standard", req, ""), true, "方案", "skipped"},
		{"defect", nil, true, "确认", "skipped"}, {"explore", layout("standard", "", design), false, "结论", "done"},
	} {
		if got := state(Flow(tc.kind, tc.files, c, tc.skip), tc.name); got != tc.want {
			t.Error(tc.kind, tc.name, got, tc.want)
		}
	}
}
