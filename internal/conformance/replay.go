// Package conformance 用一次性假内核和本地 HTTP 录制响应检查扩展协议。
package conformance

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"net/url"
	"os"
	"path/filepath"
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
type requestKey struct{ method, path, query string }

type fixture struct {
	provided  bool
	responses map[requestKey][]response
	config    json.RawMessage
	expect    struct {
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

func loadFixture(dir string) (fixture, error) {
	f := fixture{responses: map[requestKey][]response{}, config: json.RawMessage(`{}`)}
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
		case "expect.json":
			f.provided = true
			err = readFixtureFile(path, &f.expect)
		case "http":
			err = f.loadHTTP(path)
		}
		if err != nil {
			return f, err
		}
	}
	return f, nil
}

func (f *fixture) loadHTTP(dir string) error {
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
			if err := f.add(row); err != nil {
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
	return stringValues(fields)
}

func (f *fixture) add(row recording) error {
	if row.Method == "" || !strings.HasPrefix(row.Path, "/") || row.Status < 200 || row.Status > 599 || row.Body == nil {
		return errors.New("需要 method、绝对 path、最终响应 status 和 body")
	}
	query, err := queryValues(row.Query)
	if err != nil {
		return fmt.Errorf("query: %w", err)
	}
	headers, err := stringValues(row.Headers)
	if err != nil {
		return fmt.Errorf("headers: %w", err)
	}
	body := []byte(row.Body)
	if body[0] == '"' {
		var str string
		if err := jsonv2.Unmarshal(body, &str); err != nil {
			return err
		}
		body = []byte(str)
	}
	key := requestKey{row.Method, row.Path, query.Encode()}
	f.provided = true
	f.responses[key] = append(f.responses[key], response{row.Status, http.Header(headers), body})
	return nil
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
	positions map[requestKey]int
	gate      *httpGate
}

type httpGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func startReplay(f fixture, tr *transcript, timeout time.Duration) (*replay, error) {
	r := &replay{f: f, trace: tr, positions: map[requestKey]int{}}
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
	key := requestKey{q.Method, q.URL.Path, values.Encode()}
	r.mu.Lock()
	rows := r.f.responses[key]
	if len(rows) == 0 || err != nil {
		r.mu.Unlock()
		r.trace.http(q, 599, false, "未匹配录制响应")
		http.Error(w, "未匹配 fixture", 599)
		return
	}
	// 同一键的多条录制按文件及数组顺序消费，最后一条可重复；每轮从第一条重放。
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
