package method

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

const formatVersion = 1

var frontLine = regexp.MustCompile(`^([A-Za-z_][\pL\pN_\-]*):(?:[ \t]+(.*))?$`)
var integer = regexp.MustCompile(`^[+-]?[0-9]+$`)

// Field 按原顺序保留文件头字段，供正文之外的元数据读取和回写。
type Field struct {
	Key   string
	Value any
}

func (d Document) value(key string) any {
	for _, f := range d.Front {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// 缺结束行时将整篇交给正文解析，避免吞掉后续章节。
func (d *Document) readFront(lines []string) int {
	if strings.TrimRightFunc(lines[0], unicode.IsSpace) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRightFunc(lines[i], unicode.IsSpace) == "---" {
			d.Front = make([]Field, 0, i-1)
			seen := make(map[string]bool)
			for j, line := range lines[1:i] {
				d.frontField(line, j+2, seen)
			}
			return i + 1
		}
	}
	return 0
}

func (d *Document) frontField(line string, n int, seen map[string]bool) {
	line = strings.TrimRightFunc(line, unicode.IsSpace)
	if line == "" {
		return
	}
	m := frontLine.FindStringSubmatch(line)
	if m == nil {
		d.issue("F1 front", n, "文件头缺少键值对")
		return
	}
	key := m[1]
	v, err := scalar(m[2])
	if key == "craft" && (err != nil || v != formatVersion) {
		d.unsupported = true
		d.issue("F1 front", n, fmt.Sprintf("不支持的 craft 格式版本 %q", m[2]))
	}
	if seen[key] {
		d.issue("F1 front", n, "文件头键重复："+key)
		return
	}
	if err != nil {
		d.issue("F1 front", n, key+"："+err.Error())
		return
	}
	seen[key] = true
	d.Front = append(d.Front, Field{key, v})
}

func scalar(raw string) (any, error) {
	s := strings.TrimSpace(raw)
	switch {
	case s == "":
		return nil, nil
	case s[0] == '"' || s[0] == '\'':
		v, rest, err := quoted(s)
		if err == nil && rest != "" {
			err = fmt.Errorf("引号之后有多余内容")
		}
		return v, err
	case s[0] == '[':
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("列表没有闭合")
		}
		return scalarList(s[1 : len(s)-1])
	default:
		return bare(s)
	}
}

func bare(s string) (any, error) {
	switch s {
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "null":
		return nil, nil
	}
	if integer.MatchString(s) {
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, fmt.Errorf("整数超出范围：%w", err)
		}
		return n, nil
	}
	if strings.ContainsRune("{|>&*!%@`#[]", rune(s[0])) {
		return nil, fmt.Errorf("不支持的标量 %q", s)
	}
	return s, nil
}

func quoted(s string) (string, string, error) {
	var value strings.Builder
	quote := s[0]
	for i := 1; i < len(s); i++ {
		switch {
		case s[i] == quote:
			if quote == '\'' && i+1 < len(s) && s[i+1] == '\'' {
				value.WriteByte(quote)
				i++
				continue
			}
			return value.String(), s[i+1:], nil
		case quote == '"' && s[i] == '\\':
			i++
			if i >= len(s) {
				break
			}
			switch s[i] {
			case '\\', '"':
				value.WriteByte(s[i])
			case 'n':
				value.WriteByte('\n')
			case 't':
				value.WriteByte('\t')
			default:
				return "", "", fmt.Errorf("不支持的转义 %q", s[i])
			}
		default:
			value.WriteByte(s[i])
		}
	}
	return "", "", fmt.Errorf("引号没有闭合")
}

func scalarList(s string) ([]any, error) {
	s = strings.TrimSpace(s)
	items := []any{}
	if s == "" {
		return items, nil
	}
	for {
		var v any
		var err error
		if s == "" {
			return nil, fmt.Errorf("列表末尾多出逗号")
		}
		if s[0] == '\'' || s[0] == '"' {
			v, s, err = quoted(s)
		} else {
			i := strings.IndexByte(s, ',')
			if i < 0 {
				i = len(s)
			}
			item := strings.TrimSpace(s[:i])
			if item == "" {
				return nil, fmt.Errorf("列表中有空元素")
			}
			v, err = bare(item)
			s = s[i:]
		}
		if err != nil {
			return nil, err
		}
		items = append(items, v)
		s = strings.TrimLeft(s, " \t")
		if s == "" {
			return items, nil
		}
		if s[0] != ',' {
			return nil, fmt.Errorf("列表元素之间缺逗号")
		}
		s = strings.TrimLeft(s[1:], " \t")
	}
}
