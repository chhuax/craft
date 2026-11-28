package method

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

func cells(line string) []string {
	line = strings.TrimPrefix(strings.TrimSpace(line), "|")
	if !strings.HasSuffix(line, "\\|") {
		line = strings.TrimSuffix(line, "|")
	}
	var row []string
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] == '|' && (i == 0 || line[i-1] != '\\') {
			row = append(row, cellText(line[start:i]))
			start = i + 1
		}
	}
	return append(row, cellText(line[start:]))
}

func cellText(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\\|", "|"))
}

var alignment = regexp.MustCompile(`^:?-+:?$`)
var closing = regexp.MustCompile("^收尾[：:]\\s*`([^`]+)`")

func separator(line string) bool {
	for _, c := range cells(line) {
		if !alignment.MatchString(c) {
			return false
		}
	}
	return true
}

// 只取节内第一张表。错误表头也是第一张，避免用后面的示例掩盖正文的问题。
func (d *Document) readTable(name string, first int, lines []string) {
	verify := name == "验证"
	header := []string{"编号", "场景", "预期"}
	rule := "F3 scenarios"
	if verify {
		header = []string{"场景", "证据", "方式"}
		rule = "F6 coverage"
	}
	var code, found bool
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if fenced(line) {
			code = !code
			continue
		}
		if code {
			continue
		}
		if verify && d.ClosingCommand == "" {
			if m := closing.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				d.ClosingCommand = strings.TrimSpace(m[1])
			}
		}
		if found || i+1 >= len(lines) || !strings.Contains(line, "|") || !separator(lines[i+1]) {
			continue
		}
		found = true
		if !slices.Equal(cells(line), header) {
			d.issue(rule, first+i, "「"+name+"」表头不符，约定为 "+strings.Join(header, " | "))
			continue
		}
		if verify {
			d.Verification = []VerificationRow{}
		} else {
			d.Scenarios = []Scenario{}
		}
		i++
		for i+1 < len(lines) && !fenced(lines[i+1]) && strings.Contains(lines[i+1], "|") {
			i++
			row := cells(lines[i])
			for len(row) < 3 {
				row = append(row, "")
			}
			if verify {
				ids := strings.FieldsFunc(row[0], func(r rune) bool { return unicode.IsSpace(r) || r == '、' || r == ',' || r == '，' })
				d.Verification = append(d.Verification, VerificationRow{ids, row[1], row[2], first + i})
			} else {
				s := Scenario{ID: row[0], Text: row[1], Expected: row[2], Line: first + i}
				s.Unchanged = strings.HasPrefix(s.Text, "不变：") || strings.HasPrefix(s.Text, "不变:")
				d.Scenarios = append(d.Scenarios, s)
			}
		}
	}
}
