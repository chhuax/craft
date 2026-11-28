package conformance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var instant = time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t testing.TB, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	must(t, err)
	must(t, os.MkdirAll(filepath.Dir(path), 0700))
	must(t, os.WriteFile(path, data, 0600))
}

func extension(t testing.TB, mode string) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample")
	t.Setenv("CRAFT_HOME", t.TempDir())
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	name := "sample" + filepath.Ext(os.Args[0])
	alarms := []any{}
	for _, id := range []string{"items", "auth", "limit", "down"} {
		alarms = append(alarms, map[string]string{"id": id, "every": "1h"})
	}
	writeJSON(t, filepath.Join(dir, "manifest.json"), map[string]any{
		"env": []string{"GORACE"}, "id": "sample", "version": "1", "protocol": 1, "exec": []string{name, "--craft-conformance-helper", mode},
		"alarms": alarms, "watch_sources": []any{map[string]any{"id": "feed",
			"thread_kinds": []any{map[string]any{"id": "item", "object_type": "feed.item", "closable": true}},
			"events":       []any{map[string]string{"id": "change", "default": "observe", "strength": "explicit"}}}},
	})
	src, err := os.Open(os.Args[0])
	must(t, err)
	defer func() { must(t, src.Close()) }()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE, 0700)
	must(t, err)
	_, err = io.Copy(dst, src)
	must(t, dst.Close())
	must(t, err)
	fx := filepath.Join(dir, "testdata", "conformance")
	var ids, objects []string
	for n := range 10 {
		id := fmt.Sprintf("obj:%d", n)
		ids = append(ids, "item:"+id)
		objects = append(objects, id)
	}
	since := instant.Add(-7 * 24 * time.Hour).Format(time.RFC3339)
	if mode == "window" {
		// 上游请求的实际起点越界。即便命中通配录制，检查 ① 也必须报告失败。
		since = "*"
	}
	writeJSON(t, filepath.Join(fx, "http", "items.json"), map[string]any{"method": "GET", "path": "/items", "query": map[string]string{"since": since}, "status": 200, "headers": map[string]string{"X-Recorded": "yes"}, "body": objects})
	var failures []any
	for path, status := range map[string]int{"/auth": 401, "/limit": 429, "/down": 503} {
		failures = append(failures, map[string]any{"method": "GET", "path": path, "status": status, "body": ""})
	}
	writeJSON(t, filepath.Join(fx, "http", "errors.json"), failures)
	writeJSON(t, filepath.Join(fx, "config.json"), map[string]any{"urls": []string{"${CRAFT_FIXTURE_HTTP}/"}})
	writeJSON(t, filepath.Join(fx, "expect.json"), map[string]any{"threadIds": ids, "lookback": map[string]string{"param": "since"}})
	return dir, fx
}

func TestReplayQueryMatching(t *testing.T) {
	dir := t.TempDir()
	writeJSON(t, filepath.Join(dir, "http", "items.json"), []any{
		map[string]any{"method": "GET", "path": "/items", "query": map[string]string{"page": "1", "since": "*"}, "status": 200, "body": "first"},
		map[string]any{"method": "GET", "path": "/items", "query": map[string]string{"page": "1", "since": "*"}, "status": 200, "body": "second"},
		map[string]any{"method": "GET", "path": "/items", "query": map[string]string{"page": "1", "since": "fixed"}, "status": 200, "body": "exact"},
		map[string]any{"method": "GET", "path": "/items", "query": map[string]any{"tag": []string{"one", "two"}}, "status": 200, "body": "both"},
	})
	f, err := loadFixture(dir, instant)
	must(t, err)
	r := &replay{f: f, trace: &transcript{}, positions: map[int]int{}}
	for _, tc := range []struct {
		query, body string
		status      int
	}{
		{"since=fixed&page=1", "exact", 200},
		{"page=1&since=today", "first", 200},
		{"since=&page=1", "second", 200},
		{"page=2&since=today", "", 599},
		{"page=1", "", 599},
		{"page=1&other=today", "", 599},
		{"since=today&page=1&extra=y", "", 599},
		{"tag=one&tag=two", "both", 200},
		{"tag=two&tag=one", "", 599},
	} {
		req := httptest.NewRequest("GET", "/items?"+tc.query, nil)
		w := httptest.NewRecorder()
		r.serve(w, req)
		if w.Code != tc.status || tc.status == 200 && w.Body.String() != tc.body {
			t.Errorf("%s 得到 %d %q", tc.query, w.Code, w.Body.String())
		}
		recorded := r.trace.events[len(r.trace.events)-1].query.Encode()
		if recorded != req.URL.Query().Encode() {
			t.Errorf("轨迹里的实际查询变成了 %q，原请求为 %q", recorded, tc.query)
		}
	}
}

func TestFixtureTimes(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 15, 20, 0, 0, 123456789, time.FixedZone("UTC+8", 8*60*60))
	const stamp = "2026-01-15T12:00:00.123Z"
	const token = "${CRAFT_FIXTURE_NOW}"
	const other = "${CRAFT_FIXTURE_NOW_LATER}"
	writeJSON(t, filepath.Join(dir, "config.json"), map[string]any{
		token: []string{token + " ${CRAFT_FIXTURE_HTTP}", other},
	})
	const notes = "引号\"、换行\n与控制字符\x07"
	writeJSON(t, filepath.Join(dir, "http", "data.json"), map[string]any{
		"method": "GET", "path": "/data", "query": map[string]string{"at": token}, "status": 200,
		"headers": map[string][]string{"Moment": {token, "${CRAFT_FIXTURE_NOW+1h}"}},
		"body":    map[string]any{"notes": notes + token + "${CRAFT_FIXTURE_NOW-4m59.5s}", "id": json.Number("9007199254740993")},
	})
	f, err := loadFixture(dir, now)
	must(t, err)
	config, err := f.configuration("http://fixture")
	must(t, err)
	var values map[string][]string
	must(t, json.Unmarshal(config, &values))
	if !slices.Equal(values[token], []string{stamp + " http://fixture", other}) {
		t.Fatal(values)
	}
	route := f.routes[0]
	row := route.rows[0]
	if route.query.Get("at") != token || !slices.Equal(row.header.Values("Moment"), []string{stamp, "2026-01-15T13:00:00.123Z"}) {
		t.Fatal(route.query, row.header)
	}
	var body struct {
		Notes string
		ID    int64
	}
	must(t, json.Unmarshal(row.body, &body))
	if body.Notes != notes+stamp+"2026-01-15T11:55:00.623Z" || body.ID != 9007199254740993 {
		t.Fatal(body)
	}
}

func TestFixtureTimeErrorsIdentifyLocation(t *testing.T) {
	const bad = "${CRAFT_FIXTURE_NOW+soon}"
	for _, part := range []string{"config", "headers", "body"} {
		t.Run(part, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "http", "data.json")
			row := map[string]any{"method": "GET", "path": "/data", "status": 200, "body": "ok"}
			var fixture any = []any{row}
			if part == "config" {
				path = filepath.Join(dir, "config.json")
				fixture = map[string]any{"when": []string{bad}}
			} else {
				row[part] = map[string]any{"when": []string{bad}}
			}
			writeJSON(t, path, fixture)
			_, err := loadFixture(dir, instant)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "when") || !strings.Contains(err.Error(), "+soon") {
				t.Fatalf("缺少文件、字段位置或非法偏移: %v", err)
			}
		})
	}
}

func TestFixtureTimeIDsAcrossRounds(t *testing.T) {
	dir, fx := extension(t, "good")
	const stamp = "2026-01-15T12:00:00.000Z"
	writeJSON(t, filepath.Join(fx, "http", "items.json"), map[string]any{
		"method": "GET", "path": "/items", "query": map[string]string{"since": "*"}, "status": 200,
		"headers": map[string]string{"X-Recorded": "yes"}, "body": []string{"change@${CRAFT_FIXTURE_NOW}"},
	})
	writeJSON(t, filepath.Join(fx, "expect.json"), map[string]any{
		"threadIds": []string{"item:change@" + stamp}, "lookback": map[string]string{"param": "since"},
	})
	calls := 0
	checks := Run(t.Context(), dir, Options{Fixtures: fx, Now: func() time.Time {
		calls++
		return instant.Add(time.Duration(calls-1) * time.Hour)
	}})
	checkStatuses(t, checks, nil, nil)
	if calls != 1 {
		t.Fatalf("一次重放测试读取了 %d 次时钟", calls)
	}
}

func exercise(t testing.TB, dir, fx string, timeout time.Duration) []Check {
	t.Helper()
	return Run(t.Context(), dir, Options{Fixtures: fx, Now: func() time.Time { return instant }, Timeout: timeout, CancelGrace: 250 * time.Millisecond})
}

func checkStatuses(t testing.TB, checks []Check, failures, skips []string) {
	t.Helper()
	found := map[string]bool{}
	for _, c := range checks {
		want := "pass"
		switch {
		case slices.Contains(failures, c.Name):
			want = "fail"
		case slices.Contains(skips, c.Name):
			want = "skip"
		}
		if found[c.Name] || c.Status != want || c.Status != "pass" && c.Detail == "" {
			t.Errorf("%+v，期望 %s", c, want)
		}
		found[c.Name] = true
	}
	for _, name := range append([]string{"manifest", "initialize", "http", "alarm/run", "§5.1 游标", "§5.2 状态事实", "§5.3 来源", "§5.4 事件标识", "§5.5 facts", "§5.6 对象标识", "§5.7 错误映射", "§5.8 取消", "§5.9 严格 JSON"}, failures...) {
		if !found[name] {
			t.Errorf("缺少检查 %s", name)
		}
	}
}

func TestConformance(t *testing.T) {
	for _, tc := range []struct{ mode, fail string }{
		{"good", ""}, {"cursor", "§5.1 游标"}, {"window", "§5.1 游标"}, {"order", "§5.2 状态事实"}, {"late-describe", "§5.2 状态事实"},
		{"foreign", "§5.3 来源"}, {"unstable", "§5.4 事件标识"}, {"facts", "§5.5 facts"},
		{"identity", "§5.6 对象标识"}, {"auth", "§5.7 错误映射"}, {"limit", "§5.7 错误映射"},
		{"down", "§5.7 错误映射"}, {"close", "§5.7 错误映射"}, {"cancel", "§5.8 取消"}, {"nan", "§5.9 严格 JSON"},
		{"unmatched", "http"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			dir, _ := extension(t, tc.mode)
			var failures []string
			if tc.fail != "" {
				failures = []string{tc.fail}
			}
			results := exercise(t, dir, "", 3*time.Second)
			checkStatuses(t, results, failures, nil)
			if tc.mode == "window" && !strings.Contains(results[cursor].Detail, instant.Add(-7*24*time.Hour-time.Second).Format(time.RFC3339)) {
				t.Fatal("窗口诊断缺少请求的实际起点", results[cursor])
			}
		})
	}
}

func TestMissingExpectations(t *testing.T) {
	dir, fx := extension(t, "good")
	must(t, os.Remove(filepath.Join(fx, "expect.json")))
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), nil, []string{"§5.1 游标", "§5.6 对象标识"})
}

func TestEmptyFixtures(t *testing.T) {
	dir, _ := extension(t, "good")
	empty := t.TempDir()
	for _, fx := range []string{empty, filepath.Join(empty, "absent")} {
		for _, c := range exercise(t, dir, fx, 3*time.Second) {
			if c.Name == "manifest" || c.Name == "initialize" {
				if c.Status != "pass" {
					t.Fatal(c)
				}
			} else if c.Status != "skip" || !strings.Contains(c.Detail, "fixture") {
				t.Fatal(c)
			}
		}
	}
}

func TestProcessFailures(t *testing.T) {
	for _, tc := range []struct{ mode, check string }{{"timeout", "initialize"}, {"accept-major", "initialize"}, {"crash", "alarm/run"}, {"invalid", "§5.9 严格 JSON"}, {"long", "alarm/run"}} {
		t.Run(tc.mode, func(t *testing.T) {
			dir, _ := extension(t, tc.mode)
			timeout := 3 * time.Second
			if tc.mode == "timeout" {
				timeout = 250 * time.Millisecond
			}
			checks := exercise(t, dir, "", timeout)
			if !slices.ContainsFunc(checks, func(c Check) bool { return c.Name == tc.check && c.Status == "fail" }) {
				t.Fatal(checks)
			}
		})
	}
}

func BenchmarkConformanceTenObjects(b *testing.B) {
	dir, _ := extension(b, "good")
	for b.Loop() {
		checkStatuses(b, exercise(b, dir, "", 3*time.Second), nil, nil)
	}
}

func TestExpectedObjectMissing(t *testing.T) {
	dir, fx := extension(t, "good")
	var want struct {
		ThreadIDs []string          `json:"threadIds"`
		Lookback  map[string]string `json:"lookback"`
	}
	must(t, readFixtureFile(filepath.Join(fx, "expect.json"), &want))
	want.ThreadIDs = append(want.ThreadIDs, "item:absent")
	writeJSON(t, filepath.Join(fx, "expect.json"), want)
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), []string{"§5.6 对象标识"}, nil)
}

func TestMalformedFactAndErrorEvidence(t *testing.T) {
	for _, tc := range []struct{ mode, check string }{{"flat", "§5.5 facts"}, {"bad-block", "§5.7 错误映射"}} {
		t.Run(tc.mode, func(t *testing.T) {
			dir, fx := extension(t, tc.mode)
			checkStatuses(t, exercise(t, dir, fx, 3*time.Second), []string{tc.check}, nil)
		})
	}
}

func TestWindowDuringInitialize(t *testing.T) {
	dir, fx := extension(t, "init-window")
	writeJSON(t, filepath.Join(fx, "http", "init.json"), map[string]any{"method": "GET", "path": "/init", "query": "since=2000-01-01", "status": 200, "body": "ok"})
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), []string{"§5.1 游标"}, nil)
}

func TestKernelRepliesOverStdio(t *testing.T) {
	dir, fx := extension(t, "reads")
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), nil, nil)
}

func TestFixtureWithConfigButNoRecordings(t *testing.T) {
	dir, fx := extension(t, "good")
	must(t, os.RemoveAll(filepath.Join(fx, "http")))
	checks := exercise(t, dir, fx, 3*time.Second)
	if !slices.ContainsFunc(checks, func(c Check) bool { return c.Name == "http" && c.Status == "fail" && strings.Contains(c.Detail, "599") }) {
		t.Fatal(checks)
	}
}

func TestPrivateKeyAfterAllReceipts(t *testing.T) {
	dir, fx := extension(t, "private-cursor")
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), nil, nil)
}

func TestOversizedFrameCannotPassStrictJSON(t *testing.T) {
	dir, fx := extension(t, "long")
	checks := exercise(t, dir, fx, 3*time.Second)
	if !slices.ContainsFunc(checks, func(c Check) bool { return c.Name == "§5.9 严格 JSON" && c.Status == "fail" }) {
		t.Fatal(checks)
	}
}

func TestCallsOutsideAlarm(t *testing.T) {
	dir, fx := extension(t, "init-foreign")
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), []string{"§5.3 来源"}, nil)
}

func TestShortUnchangedRunDoesNotProveCancellation(t *testing.T) {
	dir, fx := extension(t, "short")
	checkStatuses(t, exercise(t, dir, fx, 3*time.Second), nil, []string{"§5.8 取消"})
}
