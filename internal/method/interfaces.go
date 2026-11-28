package method

import (
	"fmt"
	"maps"
	"regexp"
	"strings"
)

// InterfaceFile 定位同一能力的一份现状正文，Kind 为 api 或 table，不依赖文件命名或存放位置。
type InterfaceFile struct{ Capability, Kind string }

type interfaceDelta struct {
	file              InterfaceFile
	action, key, body string
}

var apiKey = regexp.MustCompile(`^(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+(/\S*)$`)
var tableKey = regexp.MustCompile(`^[A-Za-z_][\pL\pN_.]*$`)

func deltaHeading(capability, title string) (interfaceDelta, error) {
	f := strings.Fields(title)
	if capability == "" || len(f) < 2 {
		return interfaceDelta{}, fmt.Errorf("增量标题缺少能力或键：%q", title)
	}
	d := interfaceDelta{file: InterfaceFile{Capability: capability}, action: f[0], key: strings.Join(f[1:], " ")}
	if d.action == "配置" {
		d.file.Kind = "config"
		return d, nil
	}
	switch d.action {
	case "新增", "修改", "删除":
	default:
		return d, fmt.Errorf("未知的增量动作 %q", d.action)
	}
	switch {
	case apiKey.MatchString(d.key):
		d.file.Kind = "api"
	case tableKey.MatchString(d.key):
		d.file.Kind = "table"
	default:
		return d, fmt.Errorf("无效的接口或表名 %q", d.key)
	}
	return d, nil
}

func interfaceDeltas(design Document) ([]interfaceDelta, error) {
	if design.unsupported || (design.hash == "" && len(design.Issues) > 0) {
		return nil, fmt.Errorf("无法从格式有误的方案提取增量")
	}
	var content string
	found := false
	for _, s := range design.Sections {
		if s.Name != "接口与库表变更" {
			continue
		}
		if found {
			return nil, fmt.Errorf("方案含多节接口与库表变更")
		}
		found, content = true, s.Content
	}
	if content == "" {
		return nil, nil
	}
	lines := strings.Split(content, "\n")
	var capability string
	var deltas []interfaceDelta
	seen := map[[2]string]bool{}
	var code, active bool
	start := 0
	flush := func(i int) {
		if active {
			deltas[len(deltas)-1].body = strings.TrimSpace(strings.Join(lines[start:i], "\n"))
			active = false
		}
	}
	for i, line := range lines {
		if fenced(line) {
			code = !code
			continue
		}
		if code {
			continue
		}
		if title, ok := strings.CutPrefix(line, "### "); ok {
			flush(i)
			capability = strings.TrimSpace(title)
			if !safeComponent(capability) || strings.HasPrefix(capability, ".") {
				return nil, fmt.Errorf("能力名 %q 无效", capability)
			}
		}
		if title, ok := strings.CutPrefix(line, "#### "); ok {
			flush(i)
			d, err := deltaHeading(capability, title)
			if err != nil {
				return nil, err
			}
			id := [2]string{d.file.Kind, d.key}
			if seen[id] {
				return nil, fmt.Errorf("增量键 %q 重复", d.key)
			}
			seen[id] = true
			deltas = append(deltas, d)
			start, active = i+1, true
		}
	}
	flush(len(lines))
	if code || len(deltas) == 0 {
		return nil, fmt.Errorf("接口与库表变更缺少完整条目")
	}
	return deltas, nil
}

type specText struct {
	changed bool
	head    string
	order   []string
	entries map[string]string
}

func parseSpec(raw string) (specText, error) {
	raw = strings.ReplaceAll(strings.TrimPrefix(raw, "\ufeff"), "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	spec := specText{entries: map[string]string{}}
	start := 0
	var key string
	flush := func(end int) {
		block := strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n")
		if key == "" {
			spec.head = block
		} else {
			spec.entries[key] = block
		}
	}
	var code, comment bool
	for i, line := range lines {
		if !comment && fenced(line) {
			code = !code
			continue
		}
		if code {
			continue
		}
		visible := withoutComments(line, &comment)
		if !strings.HasPrefix(visible, "## ") {
			continue
		}
		flush(i)
		key = strings.Join(strings.Fields(visible[3:]), " ")
		if _, exists := spec.entries[key]; exists || key == "" {
			return spec, fmt.Errorf("现状文档的键 %q 为空或重复", key)
		}
		spec.order = append(spec.order, key)
		start = i
	}
	if code || comment {
		return spec, fmt.Errorf("现状文档围栏或注释未闭合")
	}
	flush(len(lines))
	return spec, nil
}

func (s specText) render() string {
	var parts []string
	if s.head != "" {
		parts = append(parts, s.head)
	}
	for _, key := range s.order {
		if block, ok := s.entries[key]; ok {
			parts = append(parts, block)
		}
	}
	return strings.Join(parts, "\n\n") + "\n"
}

func (s *specText) apply(d interfaceDelta, change string) error {
	old, exists := s.entries[d.key]
	if d.action == "删除" {
		if exists {
			s.changed = true
			delete(s.entries, d.key)
		}
		return nil
	}
	block := "## " + d.key + "\n\n"
	if d.body != "" {
		block += d.body + "\n\n"
	}
	block += "> 最近变更：" + change
	// 重放只接受同一份结果；即使标记相同，人工补过的正文也不能被一次新增覆盖。
	if d.action == "新增" && exists && old != block {
		return fmt.Errorf("新增 %s 与 %s 的已有条目冲突", d.key, d.file.Capability)
	}
	if exists && old == block {
		return nil
	}
	s.changed = true
	if !exists {
		s.order = append(s.order, d.key)
	}
	s.entries[d.key] = block
	return nil
}

// MergeInterfaces 从调用方提供的基线正文重算各文档；同一增量可重放，任一冲突返回完整原输入。
// 配置项留在方案里。返回的文本是否创建为文件，由维护该文档的调用方决定。
func MergeInterfaces(base map[InterfaceFile]string, design Document, change string) (map[InterfaceFile]string, error) {
	if !safeComponent(change) {
		return base, fmt.Errorf("变更名无效：%q", change)
	}
	deltas, err := interfaceDeltas(design)
	if err != nil {
		return base, err
	}
	specs := map[InterfaceFile]*specText{}
	for _, d := range deltas {
		if d.file.Kind == "config" {
			continue
		}
		s := specs[d.file]
		if s == nil {
			raw, exists := base[d.file]
			if !exists && d.action == "删除" {
				continue
			}
			if !exists {
				kind := "接口"
				if d.file.Kind == "table" {
					kind = "库表"
				}
				raw = "# " + d.file.Capability + " · " + kind + "现状\n"
			}
			parsed, err := parseSpec(raw)
			if err != nil {
				return base, fmt.Errorf("%s %s：%w", d.file.Capability, d.file.Kind, err)
			}
			s = &parsed
			specs[d.file] = s
		}
		if err := s.apply(d, change); err != nil {
			return base, err
		}
	}
	out := make(map[InterfaceFile]string, len(base)+len(specs))
	maps.Copy(out, base)
	for file, s := range specs {
		if s.changed {
			out[file] = s.render()
		}
	}
	return out, nil
}
