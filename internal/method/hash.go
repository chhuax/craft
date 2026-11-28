package method

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// 确认哈希按原始字节计算：移除一个 UTF-8 BOM，把 CRLF 换成 LF，但保留单独的 CR。
// 只有严格以 "---\n" 开头且存在独占一行的结束符（可带尾随空格、制表符）才去掉文件头。
// 行首可缩进的 "- [x]"、"- [X]" 按未勾选计算；其余空白、尾换行和非 UTF-8 字节均原样参与 SHA-256。
func normalizedHash(text string) string {
	text = strings.ReplaceAll(strings.TrimPrefix(text, "\ufeff"), "\r\n", "\n")
	if rest, ok := strings.CutPrefix(text, "---\n"); ok {
		for rest != "" {
			line, tail, _ := strings.Cut(rest, "\n")
			if strings.TrimRight(line, " \t") == "---" {
				text = tail
				break
			}
			rest = tail
		}
	}
	data := []byte(text)
	offset := 0
	for line := range strings.SplitSeq(text, "\n") {
		body := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(body, "- [x]") || strings.HasPrefix(body, "- [X]") {
			data[offset+len(line)-len(body)+3] = ' '
		}
		offset += len(line) + 1
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
