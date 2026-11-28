package exthost

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Tail 返回扩展 stderr 最后指定行数。根目录约束同时防止路径分隔符及符号链接越界读取。
func Tail(dir, id string, lines int) (_ []string, err error) {
	if !validID(id) || strings.Contains(id, ":") || lines < 1 || lines > 1000 {
		return nil, ErrInvalidParams
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	f, err := root.Open(id + ".log")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("日志不是普通文件")
	}
	raw, err := io.ReadAll(io.NewSectionReader(f, max(0, info.Size()-logLimit), logLimit))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(raw) == 0 {
		return []string{}, nil
	}
	parts := bytes.Split(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n"))
	result := make([]string, 0, min(len(parts), lines))
	for _, line := range parts[max(0, len(parts)-lines):] {
		result = append(result, strings.ToValidUTF8(string(bytes.TrimSuffix(line, []byte("\r"))), "�"))
	}
	return result, nil
}
