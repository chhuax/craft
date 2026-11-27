package engine

import "testing"

func TestKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		want   string
	}{
		{"空数组", nil, "4f53cda18c2baa0c0354bb5f9a3ecbe5"},
		{"空字符串", []string{""}, "055539df4a0b804c58caf46c0cd2941a"},
		{"工作标识", []string{"work", "eval-7"}, "cee12fdcf631d585c130f82796bc63b6"},
		{"保留原文", []string{"事件", "<&>", "🙂"}, "478f790f1662dc08ca9b78eab4869f6c"},
		{"分隔符与字面转义", []string{"\u2028", "\u2029", "\\u2028", "\\u2029"}, "148a58d0ffd07d4b929f195690354c02"},
		{"必需转义", []string{"a\"b", "c\nd"}, "d230a37e04b1f28f3d0b79c599635054"},
		{"正序", []string{"a", "b"}, "0473ef2dc0d324ab659d3580c1134e9d"},
		{"逆序", []string{"b", "a"}, "02d8bc3008a9bb0dcc4b86d7fd3428ce"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Key(tc.values...); got != tc.want {
				t.Errorf("Key(%q) = %q；期望 %q", tc.values, got, tc.want)
			}
		})
	}
}
