// Package conformance 用一次性假内核和本地 HTTP 录制响应检查扩展协议。
package conformance

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type recording struct {
	Method  string                     `json:"method"`
	Path    string                     `json:"path"`
	Query   json.RawMessage            `json:"query"`
	Status  int                        `json:"status"`
	Headers map[string]json.RawMessage `json:"headers"`
	Body    json.RawMessage            `json:"body"`
}

type response struct {
	status int
	header http.Header
	body   []byte
}

type route struct {
	method, path string
	query        url.Values // nil 值表示该键必须存在，正常查询至少有一个值（可能是空串）。
	rows         []response
}

type fixture struct {
	provided bool
	routes   []route
	config   json.RawMessage
	expect   struct {
		ThreadIDs []string `json:"threadIds"`
		Lookback  struct {
			Param string `json:"param"`
		} `json:"lookback"`
	}
}

func readFixtureFile(path string, dst any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s 须为普通文件", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := jsonv2.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// 同一次验收共用运行起点；重放响应即使跨过毫秒边界也必须有相同的事件标识。
func loadFixture(dir string, now time.Time) (fixture, error) {
	f := fixture{config: json.RawMessage(`{}`)}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		switch entry.Name() {
		case "config.json":
			f.provided = true
			err = readFixtureFile(path, &f.config)
			if err == nil {
				f.config, err = expandTimes(f.config, now)
				if err != nil {
					return f, fmt.Errorf("%s: %w", path, err)
				}
			}
		case "expect.json":
			f.provided = true
			err = readFixtureFile(path, &f.expect)
		case "http":
			err = f.loadHTTP(path, now)
		}
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func (f *fixture) loadHTTP(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var raw json.RawMessage
		if err := readFixtureFile(path, &raw); err != nil {
			return err
		}
		var rows []recording
		if raw[0] == '[' {
			err = jsonv2.Unmarshal(raw, &rows)
		} else {
			rows = make([]recording, 1)
			err = jsonv2.Unmarshal(raw, &rows[0])
		}
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		for i, row := range rows {
			if err := f.add(row, now); err != nil {
				return fmt.Errorf("%s 第 %d 条: %w", path, i+1, err)
			}
		}
	}
	return nil
}

func stringValues(fields map[string]json.RawMessage) (url.Values, error) {
	values := url.Values{}
	for k, v := range fields {
		if len(v) > 0 && v[0] == '"' {
			var value string
			if err := jsonv2.Unmarshal(v, &value); err != nil {
				return nil, err
			}
			values.Set(k, value)
		} else {
			var many []string
			if err := jsonv2.Unmarshal(v, &many); err != nil || many == nil {
				return nil, fmt.Errorf("%s 须为字符串或字符串数组", k)
			}
			values[k] = many
		}
	}
	return values, nil
}

func queryValues(raw json.RawMessage) (url.Values, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if raw[0] == '"' {
		var str string
		if err := jsonv2.Unmarshal(raw, &str); err != nil {
			return nil, err
		}
		return url.ParseQuery(str)
	}
	var fields map[string]json.RawMessage
	if err := jsonv2.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	values, err := stringValues(fields)
	if err != nil {
		return nil, err
	}
	for name, parts := range values {
		switch {
		case len(parts) == 0:
			delete(values, name) // 与完整查询串的 Encode 行为一致。
		case fields[name][0] == '"' && parts[0] == "*":
			values[name] = nil
		}
	}
	return values, nil
}

var fixtureTime = regexp.MustCompile(`\$\{CRAFT_FIXTURE_NOW(?:[+-][^}]*)?(?:\}|$)`)

func expandTimeString(s string, now time.Time) (string, error) {
	var out strings.Builder
	start := 0
	for _, span := range fixtureTime.FindAllStringIndex(s, -1) {
		tail := strings.TrimPrefix(s[span[0]:span[1]], "${CRAFT_FIXTURE_NOW")
		offset, closed := strings.CutSuffix(tail, "}")
		if !closed {
			return "", errors.New("时间占位符缺少右花括号")
		}
		at := now
		if offset != "" {
			delta, err := time.ParseDuration(offset)
			if err != nil {
				return "", fmt.Errorf("时间占位符偏移 %q 无效: %w", offset, err)
			}
			at = at.Add(delta)
		}
		out.WriteString(s[start:span[0]])
		out.WriteString(at.UTC().Format("2006-01-02T15:04:05.000Z"))
		start = span[1]
	}
	if start == 0 {
		return s, nil
	}
	out.WriteString(s[start:])
	return out.String(), nil
}

// 只替换字符串值。保留字段名、数字精度以及正文的原始排版，错误定位到 JSON 路径。
func expandTimes(raw json.RawMessage, now time.Time) (json.RawMessage, error) {
	d := jsontext.NewDecoder(bytes.NewReader(raw))
	var out bytes.Buffer
	start := int64(0)
	for {
		kind, count := d.StackIndex(d.StackDepth())
		if d.PeekKind() != '"' || kind == '{' && count%2 == 0 {
			if _, err := d.ReadToken(); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
			continue
		}
		value, err := d.ReadValue()
		if err != nil {
			return nil, err
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return nil, err
		}
		replaced, err := expandTimeString(s, now)
		if err != nil {
			return nil, fmt.Errorf("JSON 路径 %q（字节 %d）: %w", d.StackPointer(), d.InputOffset()-int64(len(value))+1, err)
		}
		if replaced != s {
			out.Write(raw[start : d.InputOffset()-int64(len(value))])
			quoted, err := json.Marshal(replaced)
			if err != nil {
				return nil, err
			}
			out.Write(quoted)
			start = d.InputOffset()
		}
	}
	if start == 0 {
		return raw, nil
	}
	out.Write(raw[start:])
	return out.Bytes(), nil
}

func (f *fixture) add(row recording, now time.Time) error {
	if row.Method == "" || !strings.HasPrefix(row.Path, "/") || row.Status < 200 || row.Status > 599 || row.Body == nil {
		return errors.New("需要 method、绝对 path、最终响应 status 和 body")
	}
	query, err := queryValues(row.Query)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	for k, v := range row.Headers {
		row.Headers[k], err = expandTimes(v, now)
		if err != nil {
			return fmt.Errorf("headers[%q]: %w", k, err)
		}
	}
	headers, err := stringValues(row.Headers)
	if err != nil {
		return fmt.Errorf("headers: %w", err)
	}
	body, err := expandTimes(row.Body, now)
	if err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if body[0] == '"' {
		var str string
		if err := jsonv2.Unmarshal(body, &str); err != nil {
			return err
		}
		body = []byte(str)
	}
	f.provided = true
	r := response{row.Status, http.Header(headers), body}
	for i := range f.routes {
		route := &f.routes[i]
		if route.method == row.Method && route.path == row.Path && maps.EqualFunc(route.query, query, slices.Equal[[]string]) {
			route.rows = append(route.rows, r)
			return nil
		}
	}
	f.routes = append(f.routes, route{row.Method, row.Path, query, []response{r}})
	return nil
}

func (f fixture) match(method, path string, actual url.Values) int {
	best, wildcards := -1, len(actual)+1
	for i, route := range f.routes {
		if route.method != method || route.path != path || len(route.query) != len(actual) {
			continue
		}
		count := 0
		matches := maps.EqualFunc(route.query, actual, func(want, got []string) bool {
			if want == nil {
				count++
				return true
			}
			return slices.Equal(want, got)
		})
		// 优先使用精确字段最多的录制；同等具体时按文件及数组顺序选取。
		if matches && count < wildcards {
			best, wildcards = i, count
		}
	}
	return best
}

// 在 JSON 字符串解码之后替换，转义拼写和嵌套数组也生效，数字按原精度往返。
func (f fixture) configuration(base string) (json.RawMessage, error) {
	var v any
	d := json.NewDecoder(bytes.NewReader(f.config))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(replaceURL(v, base))
}

func replaceURL(v any, base string) any {
	switch x := v.(type) {
	case string:
		return strings.ReplaceAll(x, "${CRAFT_FIXTURE_HTTP}", base)
	case []any:
		for i, w := range x {
			x[i] = replaceURL(w, base)
		}
	case map[string]any:
		for k, w := range x {
			x[k] = replaceURL(w, base)
		}
	}
	return v
}

type replay struct {
	server    *http.Server
	base      string
	done      chan error
	f         fixture
	trace     *transcript
	mu        sync.Mutex
	positions map[int]int
	gate      *httpGate
}

type httpGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func startReplay(f fixture, tr *transcript, timeout time.Duration) (*replay, error) {
	r := &replay{f: f, trace: tr, positions: map[int]int{}}
	// 使用显式 IPv4 回环，避免测试工具默认监听器在 IPv4 不可用时切换到其他地址。
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &http.Server{Handler: http.HandlerFunc(r.serve), ReadHeaderTimeout: timeout, ReadTimeout: timeout, IdleTimeout: timeout, MaxHeaderBytes: 64 << 10}
	var open atomic.Int32
	s.ConnState = func(c net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew:
			if open.Add(1) > 32 {
				_ = c.Close()
			}
		case http.StateClosed:
			open.Add(-1)
		}
	}
	r.server = s
	r.base = "http://" + l.Addr().String()
	r.done = make(chan error, 1)
	go func() { r.done <- s.Serve(l) }()
	return r, nil
}

func (r *replay) close() error {
	err := r.server.Close()
	if serveErr := <-r.done; !errors.Is(serveErr, http.ErrServerClosed) {
		err = errors.Join(err, serveErr)
	}
	return err
}

func (r *replay) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.positions)
}

func (r *replay) serve(w http.ResponseWriter, q *http.Request) {
	body := http.MaxBytesReader(w, q.Body, 1<<20)
	defer func() { _ = body.Close() }()
	if _, err := io.Copy(io.Discard, body); err != nil {
		r.trace.http(q, http.StatusRequestEntityTooLarge, false, err.Error())
		http.Error(w, "请求体超限或读取失败", http.StatusRequestEntityTooLarge)
		return
	}
	values, err := url.ParseQuery(q.URL.RawQuery)
	key := r.f.match(q.Method, q.URL.Path, values)
	if key < 0 || err != nil {
		r.trace.http(q, 599, false, "未匹配录制响应")
		http.Error(w, "未匹配 fixture", 599)
		return
	}
	// 同一键的多条录制按文件及数组顺序消费，最后一条可重复；每轮从第一条重放。
	r.mu.Lock()
	rows := r.f.routes[key].rows
	i := r.positions[key]
	row := rows[i]
	r.positions[key] = min(i+1, len(rows)-1)
	gate := r.gate
	r.mu.Unlock()
	empty := bytes.TrimSpace(row.body)
	r.trace.http(q, row.status, len(empty) == 0 || string(empty) == "null" || string(empty) == "[]" || string(empty) == "{}", "")
	if gate != nil {
		gate.once.Do(func() { close(gate.entered) })
		select {
		case <-gate.release:
		case <-q.Context().Done():
			return
		}
	}
	for k, v := range row.header {
		w.Header()[http.CanonicalHeaderKey(k)] = slices.Clone(v)
	}
	w.WriteHeader(row.status)
	_, _ = w.Write(row.body)
}
