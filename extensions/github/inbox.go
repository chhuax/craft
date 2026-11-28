package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/chhuax/craft/pkg/protocol"
)

type cursor struct {
	At       time.Time `json:"at"`
	ETag     string    `json:"etag,omitempty"`
	Modified string    `json:"modified,omitempty"`
}
type notification struct {
	Reason  string
	Updated time.Time `json:"updated_at"`
	Subject struct {
		Type, URL, Title string
		Comment          string `json:"latest_comment_url"`
	}
	Repository struct {
		Name string `json:"full_name"`
	}
}

func (e *extension) inbox(ctx context.Context) error {
	var last cursor
	if err := e.state(ctx, "cursor", &last); err != nil {
		return err
	}
	now := e.now()
	var after int64
	if err := e.state(ctx, "poll_after", &after); err != nil {
		return err
	}
	if now.UnixMilli() < after {
		return nil
	}
	since := now.Add(-e.window)
	if overlap := last.At.Add(-5 * time.Second); overlap.After(since) && !overlap.After(now) {
		since = overlap
	}
	// 临时文件只排队通知的寻址信息，让大窗口也能先读完分页再投递，内存只保留一页。
	spool, err := os.CreateTemp(e.stateDir, "inbox-*")
	if err != nil {
		return errors.New("无法创建临时通知队列")
	}
	defer func() { _ = spool.Close(); _ = os.Remove(spool.Name()) }()
	buffer := bufio.NewWriter(spool)
	next, unchanged, err := e.notifications(ctx, since, last, json.NewEncoder(buffer))
	if err != nil || unchanged {
		return err
	}
	if err := buffer.Flush(); err != nil {
		return fmt.Errorf("写入临时通知队列: %w", err)
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("定位临时通知队列: %w", err)
	}
	if err := e.deliverNotifications(ctx, spool, since); err != nil {
		return err
	}
	// 抓取开始时刻作为水位，翻页或投递期间到达的通知仍落在下一轮窗口中。
	next.At = now
	return e.save(ctx, "cursor", next)
}

func (e *extension) deliverNotifications(ctx context.Context, r io.Reader, since time.Time) error {
	d := json.NewDecoder(r)
	var login string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var n notification
		err := d.Decode(&n)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("读取临时通知队列: %w", err)
		}
		if login == "" {
			login, err = e.me(ctx)
			if err != nil {
				return err
			}
		}
		if err = e.translate(ctx, n, login, since); err != nil {
			return err
		}
	}
}

func (e *extension) notifications(ctx context.Context, since time.Time, last cursor, spool *json.Encoder) (cursor, bool, error) {
	hdr := http.Header{}
	if last.ETag != "" {
		hdr.Set("If-None-Match", last.ETag)
	}
	if last.Modified != "" {
		hdr.Set("If-Modified-Since", last.Modified)
	}
	q := url.Values{"since": {since.UTC().Format(time.RFC3339Nano)}, "participating": {"true"}, "all": {"true"}, "per_page": {"100"}}
	var next cursor
	for page := 1; page != 0; {
		if err := ctx.Err(); err != nil {
			return next, false, err
		}
		q.Set("page", strconv.Itoa(page))
		var batch []notification
		h, err := e.request(ctx, "GET", "/notifications?"+q.Encode(), hdr, nil, &batch)
		api, _ := errors.AsType[*apiError](err)
		unchanged := page == 1 && api != nil && api.status == 304
		if err != nil && !unchanged {
			return next, false, err
		}
		if page == 1 {
			if unchanged {
				next = last
			}
			for k, dst := range map[string]*string{"ETag": &next.ETag, "Last-Modified": &next.Modified} {
				if v := h.Get(k); v != "" && len(v) <= 512 && !strings.Contains(v, os.Getenv(e.tokenEnv)) {
					*dst = v
				}
			}
			if sec, err := strconv.ParseInt(h.Get("X-Poll-Interval"), 10, 32); err == nil && sec > 0 {
				// 轮询间隔在首页响应时就生效，即使随后读取分页或投递失败也必须遵守。
				if err := e.save(ctx, "poll_after", e.now().Add(time.Duration(sec)*time.Second).UnixMilli()); err != nil {
					return next, false, err
				}
			}
			if unchanged {
				return next, true, nil
			}
		}
		// 正文与评论在完成整组分页之后按需取。
		for _, n := range batch {
			if !n.Updated.Before(since) {
				n.Subject.Title = clip(n.Subject.Title, 4096)
				if err := spool.Encode(n); err != nil {
					return next, false, fmt.Errorf("暂存通知: %w", err)
				}
			}
		}
		more, err := linkPage(h, "next")
		if err != nil || more != 0 && more <= page {
			return next, false, errors.New("通知分页未向前推进")
		}
		page = more
		hdr = nil
	}
	return next, false, nil
}

func (n notification) object() (object, bool) {
	kind, segment := "issue", "issues"
	switch n.Subject.Type {
	case "PullRequest":
		kind, segment = "pull_request", "pulls"
	case "Issue":
	default:
		return object{}, false
	}
	u, err := url.Parse(n.Subject.URL)
	if err != nil {
		return object{}, false
	}
	_, num, ok := strings.Cut(u.Path, "/repos/"+n.Repository.Name+"/"+segment+"/")
	if !ok {
		return object{}, false
	}
	o, err := parseObject(protocol.ThreadRef{Source: "github", Kind: kind, ID: n.Repository.Name + "#" + num})
	return o, err == nil
}

// 指派和评审请求是明确授权，默认观察；评论、CI 和关闭仅跟进已有工作。
// 无订阅时保留评论和终态供回看，过滤孤立 CI 噪声。rule、default、strength 的变动影响策略版本。
func (e *extension) translate(ctx context.Context, n notification, login string, since time.Time) error {
	if n.Reason == "ci_activity" && n.Subject.Type == "CheckSuite" {
		return e.suiteEvents(ctx, n, login, since)
	}
	o, ok := n.object()
	if !ok {
		return nil
	}
	switch n.Reason {
	case "review_requested", "assign":
		if (o.kind == "issue") != (n.Reason == "assign") {
			return nil
		}
		return e.assignments(ctx, o, n, login, since)
	case "mention", "comment":
		if n.Subject.Comment == "" {
			return nil
		}
		u, err := url.Parse(n.Subject.Comment)
		if err != nil {
			return errors.New("无效的评论地址")
		}
		for _, kind := range []string{"issues", "pulls"} {
			_, id, ok := strings.Cut(u.Path, "/repos/"+o.repo+"/"+kind+"/comments/")
			number, err := strconv.ParseInt(id, 10, 64)
			if !ok || err != nil || number <= 0 || kind == "pulls" && o.kind == "issue" {
				continue
			}
			var c comment
			if _, err = e.get(ctx, "/repos/"+o.repo+"/"+kind+"/comments/"+strconv.FormatInt(number, 10), &c); err != nil {
				return err
			}
			if c.ID != number {
				return errors.New("评论标识不匹配")
			}
			return e.emitComment(ctx, o, c, login, since, n.Reason, kind)
		}
		return errors.New("评论地址与通知仓库不匹配")
	case "ci_activity", "state_change":
		d, err := e.detail(ctx, o)
		if err != nil {
			return err
		}
		if err = e.facts(ctx, o, d, n.Reason == "state_change"); err != nil {
			return err
		}
		if n.Reason == "ci_activity" && o.kind == "pull_request" {
			ci, err := e.checks(ctx, o, d.Head.SHA)
			if err != nil {
				return err
			}
			return e.ciEvents(ctx, o, ci, login, since)
		}
		return e.terminal(ctx, o, d, login, since)
	}
	return nil
}

// 工作流通知可以指向检查套件。只有 GitHub 明确关联到本仓库 PR 时才建立 ThreadRef。
func (e *extension) suiteEvents(ctx context.Context, n notification, login string, since time.Time) error {
	repo := n.Repository.Name
	if !validRepo(repo) || n.Subject.URL == "" {
		return nil
	}
	u, err := url.Parse(n.Subject.URL)
	if err != nil {
		return errors.New("无效的检查套件地址")
	}
	_, num, ok := strings.Cut(u.Path, "/repos/"+repo+"/check-suites/")
	id, err := strconv.ParseInt(num, 10, 64)
	if !ok || err != nil || id <= 0 {
		return errors.New("检查套件与通知仓库不匹配")
	}
	var suite struct {
		ID  int64
		SHA string `json:"head_sha"`
		PRs []struct {
			Number int
			URL    string
		} `json:"pull_requests"`
	}
	if _, err = e.get(ctx, "/repos/"+repo+"/check-suites/"+strconv.FormatInt(id, 10), &suite); err != nil {
		return err
	}
	if suite.ID != id {
		return errors.New("检查套件标识不符")
	}
	for _, pr := range suite.PRs {
		if err := ctx.Err(); err != nil {
			return err
		}
		item := n
		item.Subject.Type, item.Subject.URL = "PullRequest", pr.URL
		o, ok := item.object()
		if !ok || o.n != pr.Number {
			continue
		}
		d, err := e.detail(ctx, o)
		if err != nil {
			return err
		}
		if err = e.facts(ctx, o, d, false); err != nil {
			return err
		}
		if suite.SHA != d.Head.SHA {
			continue
		}
		ci, err := e.checks(ctx, o, suite.SHA)
		if err != nil {
			return err
		}
		if err = e.ciEvents(ctx, o, ci, login, since); err != nil {
			return err
		}
	}
	return nil
}

// 通知的 reason 会沿用。只有 GitHub 事件里的目标和 actor 能证明本次确有指派，正文不能证明身份。
func (e *extension) assignments(ctx context.Context, o object, n notification, login string, since time.Time) error {
	for page := 1; page != 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		var events []struct {
			ID              int64
			Event           string
			Actor, Assignee user
			Reviewer        user      `json:"requested_reviewer"`
			At              time.Time `json:"created_at"`
		}
		h, err := e.get(ctx, fmt.Sprintf("%s/events?per_page=100&page=%d", o.path("issues"), page), &events)
		if err != nil {
			return err
		}
		for _, v := range events {
			event := ""
			switch {
			case n.Reason == "review_requested" && v.Event == "review_requested" && strings.EqualFold(v.Reviewer.Login, login):
				event = "pr.review_requested"
			case n.Reason == "assign" && v.Event == "assigned" && strings.EqualFold(v.Assignee.Login, login):
				event = "issue.assigned"
			}
			if event == "" || v.ID <= 0 || v.At.Before(since) {
				continue
			}
			if err := e.emit(ctx, o, event, strconv.FormatInt(v.ID, 10), v.At, v.Actor, login, "direct", n.Subject.Title, n.Reason); err != nil {
				return err
			}
		}
		more, err := linkPage(h, "next")
		if err != nil || more != 0 && more <= page {
			return errors.New("对象事件分页无效")
		}
		page = more
	}
	return nil
}

type comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      user      `json:"user"`
	Created   time.Time `json:"created_at"`
	Submitted time.Time `json:"submitted_at"`
	State     string    `json:"state,omitempty"`
}

func (e *extension) emitComment(ctx context.Context, o object, c comment, login string, since time.Time, reason, kind string) error {
	if c.ID <= 0 || c.Created.IsZero() {
		return errors.New("评论缺少标识或时间")
	}
	if c.Created.Before(since) {
		return nil
	}
	event := "pr.review_comment"
	if o.kind == "issue" {
		event = "issue.comment"
	}
	addressed := ""
	if reason == "mention" {
		words := strings.FieldsFunc(strings.ToLower(c.Body), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '@' })
		if slices.Contains(words, "@"+strings.ToLower(login)) {
			addressed = "mention"
		}
	}
	return e.emit(ctx, o, event, kind+":"+strconv.FormatInt(c.ID, 10), c.Created, c.User, login, addressed, c.Body, reason)
}

func (e *extension) emit(ctx context.Context, o object, event, disc string, at time.Time, actor user, login, addressed, body, native string) error {
	if strings.EqualFold(actor.Login, login) {
		return nil
	}
	ref := o.ref()
	// 仓库、编号和外部事件标识均为受限字符，JSON 字符串数组与协议 key 的 UTF-8 编码一致。
	wire, err := json.Marshal([]string{"github", event, ref.ID, disc})
	if err != nil {
		return err
	}
	hash := sha256.Sum256(wire)
	// 放行名单引用不随登录名转移的账号 ID，登录名只作展示。
	who := protocol.ActorRef{Source: "github", ID: "github"}
	if actor.ID > 0 {
		who.ID = strconv.FormatInt(actor.ID, 10)
		who.Name = &actor.Login
	}
	human := actor.ID > 0 && actor.Login != "" && actor.Type == "User" && !strings.HasSuffix(strings.ToLower(actor.Login), "[bot]")
	thread, err := json.Marshal(ref.ID)
	if err != nil {
		return err
	}
	field := "reason"
	switch event {
	case "pr.merged", "pr.closed":
		field = "state"
	case "pr.ci_failed":
		field = "conclusion"
		if strings.HasPrefix(disc, "status:") {
			field = "state"
		}
	}
	raw, err := json.Marshal(native)
	if err != nil {
		return err
	}
	t := protocol.Trigger{ID: hex.EncodeToString(hash[:16]), Source: "github", Threads: []protocol.ThreadRef{ref}, Actor: who, Event: event, Human: human, At: millis(at), Text: clip(body, 4096), Meta: map[string]json.RawMessage{field: raw}}
	if o.kind == "pull_request" {
		t.Channel = &protocol.ChannelRef{Source: "github", Thread: thread}
	}
	if addressed != "" {
		t.Addressed = &addressed
	}
	var ack string
	if err = e.call(ctx, "watch/receive", t, &ack); err != nil {
		return err
	}
	if ack != "queued" && ack != "duplicate" {
		return errors.New("内核未确认接收事件")
	}
	return nil
}
