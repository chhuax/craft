package outbound

import (
	"strings"
	"testing"
)

// TestOrganizationLevel 遍历声明与组织等级的所有组合，防止收严规则反向生效。
func TestOrganizationLevel(t *testing.T) {
	for _, row := range []struct {
		base Level
		want []Level
	}{
		{Auto, []Level{Auto, Confirm, ConfirmNamed, Deny}},
		{Confirm, []Level{Confirm, Confirm, ConfirmNamed, Deny}},
		{ConfirmNamed, []Level{ConfirmNamed, ConfirmNamed, ConfirmNamed, Deny}},
		{Deny, []Level{Deny, Deny, Deny, Deny}},
	} {
		for i, rule := range []Level{Auto, Confirm, ConfirmNamed, Deny} {
			if got := stricter(row.base, rule); got != row.want[i] {
				t.Errorf("%s 收严为 %s 得到 %s", row.base, rule, got)
			}
		}
	}
}

// TestContentRules 同时覆盖应拦的片段及相邻的公开地址、文本和路径。
func TestContentRules(t *testing.T) {
	rules := DefaultContentRules()
	for _, tc := range []struct {
		text string
		hit  bool
	}{
		{"/Users/person/notes", true}, {"/home/person/file", true}, {`path C:\Users\sample`, true}, {"~/notes", true},
		{"visit https://example.org/home/news", false}, {"see docs/home/file", false},
		{"Authorization: Bearer abcd12345678", true}, {"api_key=abcd123456", true}, {"password: x", true},
		{"this token format is documented", false}, {"monkey=example", false},
		{"Traceback (most recent call last):", true}, {"goroutine 12 [running]:", true}, {"\n    at sample.fn(file.js:2:8)", true}, {"meet at noon", false},
		{"http://10.0.0.1", true}, {"192.168.2.3", true}, {"127.1.2.3:80", true}, {"172.16.0.1", true}, {"172.31.255.255", true},
		{"host:10.2.3.4", true}, {"10.2.3.4.example", false}, {"172.32.0.1", false}, {"172.15.0.1", false}, {"110.0.0.1", false}, {"192.168.999.1", false},
		{"http://box.local:80", true}, {"box.local.example", false}, {"http://localhost/x", true}, {"localhost.example", false},
		{"http://[::1]/", true}, {"http://[2001:db8::1]/", false}, {"PUBLIC 192.0.2.2", false},
	} {
		if got := rules.Matches(tc.text); got != tc.hit {
			t.Errorf("%q 命中 = %v", tc.text, got)
		}
	}
}

// BenchmarkContent4KB 在没有提前命中的 4 KiB 正文上计量整张规则表。
func BenchmarkContent4KB(b *testing.B) {
	rules := DefaultContentRules()
	body := strings.Repeat("public report ready for review. ", 128)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		if rules.Matches(body) {
			b.Fatal("误报")
		}
	}
}
