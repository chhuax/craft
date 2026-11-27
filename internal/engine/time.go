package engine

import (
	"fmt"
	"regexp"
	"time"
)

const isoMillis = "2006-01-02T15:04:05.000Z"

var (
	// 固定宽度才能按字符串排序；偏移量的越界分量会被 time.Parse 自动进位，须先排除。
	timestamp = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.,][0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)
	firstMs   = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	lastMs    = time.Date(9999, 12, 31, 23, 59, 59, 999*int(time.Millisecond), time.UTC).UnixMilli()
)

func parseUTC(value string) (time.Time, error) {
	if !timestamp.MatchString(value) {
		return time.Time{}, fmt.Errorf("时间格式或时区偏移无效 %q", value)
	}
	t, err := time.ParseInLocation(time.RFC3339Nano, value, time.UTC)
	if err != nil {
		return time.Time{}, err
	}
	t = t.UTC()
	if t.Year() < 0 || t.Year() > 9999 {
		return time.Time{}, fmt.Errorf("UTC 年份超出四位范围 %q", value)
	}
	return t, nil
}

// IsoTime 将带时区的输入规范化为可排序的 UTC 字符串，舍去不足一毫秒的部分。
func IsoTime(value string) (string, error) {
	t, err := parseUTC(value)
	if err != nil {
		return "", err
	}
	return t.Format(isoMillis), nil
}

// AddMs 在调用方给定的时间上增减毫秒；先检查四位年份的边界，以免整数溢出。
func AddMs(value string, delta int64) (string, error) {
	t, err := parseUTC(value)
	if err != nil {
		return "", err
	}
	base := t.UnixMilli()
	if delta < firstMs-base || delta > lastMs-base {
		return "", fmt.Errorf("时间 %q 增减 %d 毫秒后超出四位年份范围", value, delta)
	}
	return time.UnixMilli(base + delta).UTC().Format(isoMillis), nil
}

// DiffMs 返回终点减起点的毫秒数，分别规范化两个端点以与持久化的精度一致。
func DiffMs(end, start string) (int64, error) {
	a, err := parseUTC(end)
	if err != nil {
		return 0, err
	}
	b, err := parseUTC(start)
	if err != nil {
		return 0, err
	}
	return a.UnixMilli() - b.UnixMilli(), nil
}
