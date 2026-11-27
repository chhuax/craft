package engine

import (
	"math"
	"testing"
)

func TestIsoTime(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"整秒补零", "2024-02-29T00:00:00Z", "2024-02-29T00:00:00.000Z"},
		{"偏移跨年", "2025-01-01T01:02:03.4+08:00", "2024-12-31T17:02:03.400Z"},
		{"截取毫秒", "1969-12-31T22:59:59.999999-01:00", "1969-12-31T23:59:59.999Z"},
		{"已规范化", "2024-01-02T03:04:05.020Z", "2024-01-02T03:04:05.020Z"},
		{"空输入", "", ""},
		{"无时区", "2024-01-02T03:04:05", ""},
		{"无效日期", "2023-02-29T00:00:00Z", ""},
		{"无效偏移小时", "2024-01-02T03:04:05+24:00", ""},
		{"无效偏移分钟", "2024-01-02T03:04:05+08:60", ""},
		{"转换后年份越界", "0000-01-01T00:00:00+01:00", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := IsoTime(tc.input)
			if (err != nil) != (tc.want == "") || got != tc.want {
				t.Errorf("IsoTime(%q) = %q, %v；期望 %q", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestAddMs(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		delta       int64
		want        string
	}{
		{"负偏移", "1970-01-01T08:00:00+08:00", -1, "1969-12-31T23:59:59.999Z"},
		{"跨闰日", "2024-02-29T23:59:59.999Z", 2, "2024-03-01T00:00:00.001Z"},
		{"超过三百年", "1600-01-01T00:00:00Z", 12622780800000, "2000-01-01T00:00:00.000Z"},
		{"向上越界", "9999-12-31T23:59:59.999Z", 1, ""},
		{"向下越界", "0000-01-01T00:00:00Z", -1, ""},
		{"极大增量", "2024-01-01T00:00:00Z", math.MaxInt64, ""},
		{"极小增量", "1969-01-01T00:00:00Z", math.MinInt64, ""},
		{"无效输入", "昨天", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AddMs(tc.input, tc.delta)
			if (err != nil) != (tc.want == "") || got != tc.want {
				t.Errorf("AddMs(%q, %d) = %q, %v；期望 %q", tc.input, tc.delta, got, err, tc.want)
			}
		})
	}
}

func TestDiffMs(t *testing.T) {
	for _, tc := range []struct {
		name, end, start string
		want             int64
		invalid          bool
	}{
		{"同一时刻", "1970-01-01T08:00:00+08:00", "1970-01-01T00:00:00Z", 0, false},
		{"负差值", "1969-12-31T23:59:59.9999Z", "1970-01-01T00:00:00Z", -1, false},
		{"超过三百年", "2000-01-01T00:00:00Z", "1600-01-01T00:00:00Z", 12622780800000, false},
		{"终点无效", "", "1970-01-01T00:00:00Z", 0, true},
		{"起点无效", "1970-01-01T00:00:00Z", "", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiffMs(tc.end, tc.start)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Errorf("DiffMs(%q, %q) = %d, %v；期望 %d", tc.end, tc.start, got, err, tc.want)
			}
		})
	}
}
