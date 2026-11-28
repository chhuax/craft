package main

import (
	"bytes"
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

type kernelCall func(context.Context, string, any, any) error

type extension struct {
	base, tokenEnv, stateDir string
	window                   time.Duration
	client                   *http.Client
	call                     kernelCall
	now                      func() time.Time
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func initialize(raw json.RawMessage, call kernelCall, now func() time.Time) (*extension, error) {
	var p protocol.InitializeParams
	if json.Unmarshal(raw, &p) != nil || p.Protocol != 1 {
		return nil, invalid("需要协议主版本 1")
	}
	cfg := struct {
		APIBase  string `json:"apiBase"`
		TokenEnv string `json:"tokenEnv"`
		Days     int    `json:"pollWindowDays"`
	}{"https://api.github.com", "GITHUB_TOKEN", 7}
	if len(p.Config) > 0 && json.Unmarshal(p.Config, &cfg) != nil {
		return nil, invalid("GitHub 配置无效")
	}
	u, err := url.Parse(cfg.APIBase)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalid("apiBase 需要 HTTPS 或回环 HTTP 地址")
	}
	loopback := net.ParseIP(u.Hostname()).IsLoopback() || u.Hostname() == "localhost"
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return nil, invalid("apiBase 需要 HTTPS 或回环 HTTP 地址")
	}
	if !envName.MatchString(cfg.TokenEnv) || cfg.Days < 1 || cfg.Days > 7 {
		return nil, invalid("需要 tokenEnv 和 1 至 7 天的回看窗口")
	}
	// 重定向可能把令牌带往其他源，或把对已点名 PR 的写入转给另一对象。
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &extension{strings.TrimRight(cfg.APIBase, "/"), cfg.TokenEnv, p.StateDir, time.Duration(cfg.Days) * 24 * time.Hour, client, call, now}, nil
}

func (e *extension) handle(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "alarm/run":
		var p protocol.AlarmRunParams
		if json.Unmarshal(raw, &p) != nil || (p.AlarmID != "inbox" && p.AlarmID != "subscribed") {
			return nil, invalid("未知闹钟")
		}
		return nil, e.alarm(ctx, p.AlarmID)
	case "context/read":
		var p protocol.ContextReadParams
		if json.Unmarshal(raw, &p) != nil {
			return nil, invalid("上下文参数无效")
		}
		return e.readContext(ctx, p)
	case "action/execute":
		// 缺失或 null 的 preview 不能按布尔零值解释成已经获准执行。
		var p struct {
			protocol.ActionExecuteParams
			Preview *bool `json:"preview"`
		}
		if jsonv2.Unmarshal(raw, &p) != nil || p.Preview == nil {
			return nil, invalid("动作参数无效")
		}
		p.ActionExecuteParams.Preview = *p.Preview
		return e.execute(ctx, p.ActionExecuteParams)
	case "action/verify":
		var p protocol.ActionVerifyParams
		if json.Unmarshal(raw, &p) != nil {
			return nil, invalid("核实参数无效")
		}
		return e.verify(ctx, p)
	}
	return nil, &protocol.Error{Code: -32601, Message: "未知方法"}
}

type apiError struct {
	status     int
	retry      time.Time
	protected  bool
	permission bool
}

func (err *apiError) Error() string { return fmt.Sprintf("GitHub HTTP %d", err.status) }

func retryTime(h http.Header, now time.Time) time.Time {
	var until time.Time
	if sec, err := strconv.ParseInt(h.Get("Retry-After"), 10, 32); err == nil && sec >= 0 {
		until = now.Add(time.Duration(sec) * time.Second)
	} else if t, err := http.ParseTime(h.Get("Retry-After")); err == nil {
		until = t
	}
	if sec, err := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64); err == nil && time.Unix(sec, 0).After(until) {
		until = time.Unix(sec, 0)
	}
	return until
}

func (e *extension) request(ctx context.Context, method, path string, headers http.Header, body, dst any) (http.Header, error) {
	token := os.Getenv(e.tokenEnv)
	if token == "" {
		return nil, &apiError{status: 401}
	}
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("编码 GitHub 请求: %w", err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, bytes.NewReader(payload))
	if err != nil {
		return nil, errors.New("无效的 GitHub 请求地址")
	}
	for k, v := range headers {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("GitHub 网络请求失败")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		failure := &apiError{status: resp.StatusCode}
		if failure.status == 403 || failure.status == 429 {
			failure.retry = retryTime(resp.Header, e.now())
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// GitHub 可能在错误正文回显私有数据，只把受保护分支归类，不转发原文。
		lower := strings.ToLower(string(msg))
		// 常规权限拒绝也可能附带尚有余量的配额头。动作入口须能与真实限流分别报告。
		failure.permission = failure.status == 403 && (strings.Contains(lower, "permission") || strings.Contains(lower, "resource not accessible") || resp.Header.Get("Retry-After") == "" && resp.Header.Get("X-RateLimit-Remaining") != "" && resp.Header.Get("X-RateLimit-Remaining") != "0")
		failure.protected = strings.Contains(lower, "protected branch") || strings.Contains(lower, "branch protection") || strings.Contains(lower, "repository rule")
		return resp.Header, failure
	}
	const limit = 8 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || len(data) > limit {
		return nil, errors.New("GitHub 响应过大或读取中断")
	}
	// REST 文本可能回显凭据（含 JSON 转义）。在字段被用于标识、正文或错误之前拒绝整份响应。
	stringsOnly := jsonv2.UnmarshalFunc(func(data []byte, dest *string) error {
		if err := json.Unmarshal(data, dest); err != nil {
			return err
		}
		if strings.Contains(*dest, token) {
			return errors.New("响应含凭据")
		}
		return nil
	})
	if len(data) == 0 || bytes.Equal(bytes.TrimSpace(data), []byte("null")) || jsonv2.Unmarshal(data, dst, jsonv2.MatchCaseInsensitiveNames(true), jsonv2.WithUnmarshalers(stringsOnly)) != nil {
		return nil, errors.New("GitHub 响应为空或数据无效")
	}
	return resp.Header, nil
}

func (e *extension) get(ctx context.Context, path string, dst any) (http.Header, error) {
	return e.request(ctx, http.MethodGet, path, nil, nil, dst)
}

func (e *extension) state(ctx context.Context, key string, dst any) error {
	return e.call(ctx, "state/get", map[string]string{"key": key}, dst)
}
func (e *extension) save(ctx context.Context, key string, value any) error {
	return e.call(ctx, "state/set", map[string]any{"key": key, "value": value}, nil)
}
func (e *extension) report(ctx context.Context, entry, state, detail string) error {
	return e.call(ctx, "health/report", map[string]string{"entry": entry, "state": state, "detail": detail}, nil)
}

func (e *extension) alarm(ctx context.Context, id string) error {
	if os.Getenv(e.tokenEnv) == "" {
		return e.report(ctx, id, "paused", "缺少令牌环境变量 "+e.tokenEnv)
	}
	var blocked int64
	if err := e.state(ctx, "blocked_until", &blocked); err != nil {
		return err
	}
	if e.now().UnixMilli() < blocked {
		return nil
	}
	var err error
	if id == "inbox" {
		err = e.inbox(ctx)
	} else {
		err = e.subscribed(ctx)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if api, ok := errors.AsType[*apiError](err); ok {
		switch {
		case api.status == 401:
			return e.report(ctx, id, "paused", "GitHub 身份验证失败")
		case !api.retry.IsZero() || api.status == 429:
			until := api.retry
			if !until.After(e.now()) {
				until = e.now().Add(time.Minute)
			}
			if err := e.save(ctx, "blocked_until", until.UnixMilli()); err != nil {
				return err
			}
			return e.report(ctx, id, "paused", "GitHub 请求已限流")
		}
	}
	if err != nil {
		return errors.Join(err, e.report(ctx, id, "error", err.Error()))
	}
	return e.report(ctx, id, "ok", "")
}

type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
	ID    int64  `json:"id"`
}

func (e *extension) account(ctx context.Context) (user, error) {
	var u user
	_, err := e.get(ctx, "/user", &u)
	if err == nil && u.Login == "" {
		err = errors.New("GitHub 未返回当前登录名")
	}
	return u, err
}
func (e *extension) me(ctx context.Context) (string, error) {
	u, err := e.account(ctx)
	return u.Login, err
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9_.-]+$`)

func validRepo(repo string) bool {
	_, name, _ := strings.Cut(repo, "/")
	return len(repo) <= 150 && name != "." && name != ".." && repoPattern.MatchString(repo)
}

type object struct {
	repo string
	n    int
	kind string
}

func parseObject(ref protocol.ThreadRef) (object, error) {
	repo, num, _ := strings.Cut(ref.ID, "#")
	n, err := strconv.Atoi(num)
	if ref.Source != "github" || (ref.Kind != "pull_request" && ref.Kind != "issue") || !validRepo(repo) || err != nil || n < 1 || strconv.Itoa(n) != num {
		return object{}, invalid("需要 github 的 owner/repo#number 对象")
	}
	return object{repo, n, ref.Kind}, nil
}
func (o object) ref() protocol.ThreadRef {
	return protocol.ThreadRef{Source: "github", Kind: o.kind, ID: o.repo + "#" + strconv.Itoa(o.n)}
}
func (o object) path(kind string) string {
	return "/repos/" + o.repo + "/" + kind + "/" + strconv.Itoa(o.n)
}
func (o object) endpoint() string {
	if o.kind == "issue" {
		return o.path("issues")
	}
	return o.path("pulls")
}

type detail struct {
	Number             int
	Title, Body, State string
	HTML               string    `json:"html_url"`
	Updated            time.Time `json:"updated_at"`
	Closed             time.Time `json:"closed_at"`
	MergedAt           time.Time `json:"merged_at"`
	Merged             *bool
	MergedBy           user `json:"merged_by"`
	User               user
	Head               struct{ SHA string }
	Base               struct{ Ref string }
	Mergeable          *bool
	MergeState         string `json:"mergeable_state"`
	Draft              bool
	Milestone          *struct {
		DueOn string `json:"due_on"`
	}
}

func (e *extension) detail(ctx context.Context, o object) (detail, error) {
	var d detail
	_, err := e.get(ctx, o.endpoint(), &d)
	if err == nil && (d.Number != o.n || d.State != "open" && d.State != "closed" || o.kind == "pull_request" && d.State == "closed" && d.Merged == nil) {
		err = errors.New("GitHub 对象缺少标识或状态")
	}
	return d, err
}
func millis(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

// Link 只贡献页码。主机、路径和过滤条件始终由本次请求决定。
func linkPage(h http.Header, rel string) (int, error) {
	for _, part := range strings.Split(h.Get("Link"), ",") {
		target, attrs, ok := strings.Cut(part, ";")
		if !ok || !strings.Contains(attrs, `rel="`+rel+`"`) {
			continue
		}
		u, err := url.Parse(strings.Trim(strings.TrimSpace(target), "<>"))
		if err == nil {
			n, err := strconv.Atoi(u.Query().Get("page"))
			if err == nil && n > 0 {
				return n, nil
			}
		}
		return 0, errors.New("GitHub 分页链接无效")
	}
	return 0, nil
}
