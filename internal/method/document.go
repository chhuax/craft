package method

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Gap 标识文档中的一处缺口。Line 从 1 起算，缺文件或缺节时定位到文件开头。
type Gap struct {
	Rule, Path string
	Line       int
	Message    string
}

// Section 保留二级标题、原行号与去注释后的正文。
type Section struct {
	Name, Content string
	Line          int
}

// Scenario 是验收表中的行为与预期，不变行为由场景开头的标签识别。
type Scenario struct {
	ID, Text, Expected string
	Unchanged          bool
	Line               int
}

// VerificationRow 将一份证据及验证方式对应到一个或多个场景。
type VerificationRow struct {
	IDs              []string
	Evidence, Method string
	Line             int
}

// Entry 是假设或待确认问题。只有显式编号的假设才有 ID。
type Entry struct {
	ID, Text string
	Line     int
}

// Document 是一份文档的解析结果。表为 nil 表示未识别到表，空切片表示只有表头。
type Document struct {
	Path                   string
	Front                  []Field
	Sections               []Section
	Scenarios              []Scenario
	Verification           []VerificationRow
	Assumptions, Questions []Entry
	ClosingCommand         string
	LineCount, RuleCount   int
	Issues                 []Gap
	hash                   string
	unsupported            bool
}

// Section 返回第一个同名章节，便于调用方区分缺节与空节。
func (d Document) Section(name string) *Section {
	for i := range d.Sections {
		if d.Sections[i].Name == name {
			return &d.Sections[i]
		}
	}
	return nil
}

func (d Document) hasText(name string) bool {
	s := d.Section(name)
	return s != nil && s.Content != ""
}

func (d *Document) issue(rule string, line int, message string) {
	d.Issues = append(d.Issues, Gap{rule, d.Path, line, message})
}

// ReadDocument 从调用方的文件系统读取相对路径。读失败和编码错误同样可按文件定位。
func ReadDocument(files fs.FS, path string) Document {
	if files == nil {
		d := Document{Path: path}
		d.issue("F1 front", 1, "缺少文档文件系统")
		return d
	}
	b, err := fs.ReadFile(files, path)
	if err != nil {
		d := Document{Path: path}
		d.issue("F1 front", 1, fmt.Sprintf("文档读取失败：%v", err))
		return d
	}
	return Parse(path, string(b))
}

// Parse 解析需求、方案或合一的变更文档。path 为项目内以正斜杠分隔的相对路径。
// 遇到未知版本时保留诊断，停止解释正文。
func Parse(path, text string) Document {
	d := Document{Path: path}
	if !utf8.ValidString(text) {
		d.issue("F1 front", 1, "文档含无效的 UTF-8 字节")
		return d
	}
	d.hash = normalizedHash(text)
	text = strings.ReplaceAll(strings.ReplaceAll(strings.TrimLeft(text, "\ufeff"), "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(text, "\n")
	d.LineCount = len(lines)
	if lines[len(lines)-1] == "" {
		d.LineCount--
	}
	start := d.readFront(lines)
	if !d.unsupported {
		d.readSections(lines, start)
	}
	return d
}

func fenced(line string) bool {
	line = strings.TrimSpace(line)
	return strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~")
}

// 注释可跨行；保留行的位置，使表格和问题的行号始终指向原文。
func withoutComments(line string, inside *bool) string {
	if !*inside && !strings.Contains(line, "<!--") {
		return line
	}
	var kept strings.Builder
	for line != "" {
		if *inside {
			_, rest, end := strings.Cut(line, "-->")
			if !end {
				break
			}
			*inside = false
			line = rest
		} else {
			before, rest, open := strings.Cut(line, "<!--")
			kept.WriteString(before)
			*inside = open
			line = rest
		}
	}
	return kept.String()
}

func (d *Document) readSections(lines []string, start int) {
	var comment, code bool
	seen := make(map[string]bool)
	var name string
	var body int
	for i := start; i < len(lines); i++ {
		line := lines[i]
		if !comment && fenced(line) {
			code = !code
			continue
		}
		if code {
			continue
		}
		lines[i] = withoutComments(line, &comment)
		line = lines[i]
		if !strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "##\t") {
			continue
		}
		title := strings.TrimSpace(line[2:])
		if title == "" {
			continue
		}
		if name != "" {
			d.addSection(name, body, lines[body:i], seen)
		}
		name, body = title, i+1
	}
	if name != "" {
		d.addSection(name, body, lines[body:], seen)
	}
}

func (d *Document) addSection(name string, heading int, lines []string, seen map[string]bool) {
	d.Sections = append(d.Sections, Section{name, strings.TrimSpace(strings.Join(lines, "\n")), heading})
	if seen[name] {
		d.issue("F2 sections", heading, "节名重复："+name)
		return
	}
	seen[name] = true
	switch name {
	case "验收场景", "验证":
		d.readTable(name, heading+1, lines)
	case "假设与待确认":
		d.readEntries(heading+1, lines)
	case "业务规则":
		d.RuleCount = countRules(lines)
	}
}

var bulletItem = regexp.MustCompile(`^[-*+]\s+`)
var labelItem = regexp.MustCompile(`^(假设|待确认)[：:]\s*(.*)$`)
var assumptionID = regexp.MustCompile(`^(A-[0-9]+)(?:[ \t：:、]+|$)(.*)$`)

func (d *Document) readEntries(first int, lines []string) {
	var code bool
	for i, line := range lines {
		if fenced(line) {
			code = !code
			continue
		}
		if code {
			continue
		}
		line = strings.TrimSpace(line)
		mark := bulletItem.FindStringIndex(line)
		if mark == nil {
			continue
		}
		body := strings.TrimSpace(line[mark[1]:])
		label := labelItem.FindStringSubmatch(body)
		if label != nil {
			body = strings.TrimSpace(label[2])
		}
		if body == "" {
			continue
		}
		e := Entry{Text: body, Line: first + i}
		if label != nil && label[1] == "待确认" {
			d.Questions = append(d.Questions, e)
			continue
		}
		if id := assumptionID.FindStringSubmatch(body); id != nil {
			e.ID, e.Text = id[1], strings.TrimSpace(id[2])
		} else if label == nil {
			continue
		}

		d.Assumptions = append(d.Assumptions, e)
	}
}

var listStart = regexp.MustCompile(`^(?:[-*+]|\d+[.)])\s+`)
var headingLine = regexp.MustCompile(`^#{1,6}\s+`)

// 顶格列表项和空行分开的段落各算一条；缩进续行和围栏示例不能让分档虚高。
func countRules(lines []string) int {
	count := 0
	var code, paragraph bool
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if fenced(raw) {
			code = !code
			paragraph = false
			continue
		}
		if code || line == "" || headingLine.MatchString(line) {
			paragraph = false
			continue
		}
		if end := listStart.FindStringIndex(raw); end != nil {
			paragraph = false
			if strings.TrimSpace(raw[end[1]:]) == "" {
				continue
			}
		}
		if !paragraph {
			count++
			paragraph = true
		}
	}
	return count
}
