package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// 这两个分隔符仅在嵌入脚本时才需要转义。先匹配成对的反斜杠可保留输入中的字面转义。
var jsonSeparators = strings.NewReplacer(`\\`, `\\`, `\u2028`, "\u2028", `\u2029`, "\u2029")

// Key 对紧凑的 UTF-8 JSON 字符串数组取摘要，使不同语言的调用方能重建同一标识。
func Key(values ...string) string {
	if values == nil {
		values = []string{}
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(values); err != nil {
		// 字符串数组总能编码，内存缓冲区也不会返回写入错误。
		panic(err)
	}
	wire := jsonSeparators.Replace(strings.TrimSuffix(buf.String(), "\n"))
	sum := sha256.Sum256([]byte(wire))
	return hex.EncodeToString(sum[:16])
}
