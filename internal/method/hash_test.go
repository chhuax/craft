package method

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func TestNormalizedHash(t *testing.T) {
	for _, tc := range []struct{ raw, normal string }{
		{"\ufeffa\r\nb", "a\nb"}, {"---\nstatus: draft\n--- \t\n正文", "正文"}, {"---\n---", ""},
		{"--- \n---\n", "--- \n---\n"}, {"---\n未结束\n正文", "---\n未结束\n正文"}, {"- [x] a\n \t- [X] b", "- [ ] a\n \t- [ ] b"},
		{"正文 - [x]", "正文 - [x]"}, {"a \t\n\nb\r", "a \t\n\nb\r"}, {"\xff\xfe", "\xff\xfe"}, {"", ""},
	} {
		want := fmt.Sprintf("%x", sha256.Sum256([]byte(tc.normal)))
		if got := normalizedHash(tc.raw); got != want {
			t.Errorf("%q: %s", tc.raw, got)
		}
	}
}

func BenchmarkNormalizedHash(b *testing.B) {
	for _, size := range []int{1 << 18, 1 << 20} {
		const line = "  - [X] 保留顺序\r\n"
		data := "---\ncraft: 1\n---\n" + strings.Repeat(line, size/len(line))
		b.Run(benchmarkSize(size), func(b *testing.B) {
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			for b.Loop() {
				normalizedHash(data)
			}
		})
	}
}
