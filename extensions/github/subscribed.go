package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

type subscription struct {
	Ref    protocol.ThreadRef `json:"ref"`
	Closed *struct {
		At string `json:"at"`
	} `json:"closed"`
}
type sweep struct {
	After   string    `json:"after,omitempty"`
	Since   time.Time `json:"since"`
	Started time.Time `json:"started"`
}

func subKey(s subscription) string { return s.Ref.Kind + ":" + s.Ref.ID }

func (e *extension) subscribed(ctx context.Context) error {
	// 即使当前没有订阅，也要让此来源的凭据失效在健康面板可见。
	login, err := e.me(ctx)
	if err != nil {
		return err
	}
	var subs []subscription
	if err := e.call(ctx, "thread/subscribed", struct{}{}, &subs); err != nil || len(subs) == 0 {
		return err
	}
	var cursor sweep
	if err := e.state(ctx, "subscribed", &cursor); err != nil {
		return err
	}
	now := e.now()
	if cursor.Started.IsZero() {
		cursor.Started = now
	}
	if floor := now.Add(-e.window); cursor.Since.Before(floor) {
		cursor.Since = floor
	}
	slices.SortFunc(subs, func(a, b subscription) int { return strings.Compare(subKey(a), subKey(b)) })
	i := 0
	for i < len(subs) && subKey(subs[i]) <= cursor.After {
		i++
	}
	batch := subs[i:min(i+100, len(subs))]
	if len(batch) > 0 {
		if err = e.pollBatch(ctx, batch, login, cursor.Since); err != nil {
			return err
		}
	}
	// 上次抓取的末对象可能已退订，按稳定键续扫；整轮结束才更换时间窗，后面的对象不会饿死。
	if len(batch)+i == len(subs) {
		cursor = sweep{Since: cursor.Started.Add(-5 * time.Second)}
	} else {
		cursor.After = subKey(batch[len(batch)-1])
	}
	return e.save(ctx, "subscribed", cursor)
}

func (e *extension) pollBatch(ctx context.Context, batch []subscription, login string, since time.Time) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan subscription)
	var wg sync.WaitGroup
	for range min(4, len(batch)) {
		wg.Go(func() {
			for s := range jobs {
				if ctx.Err() != nil {
					return
				}
				if err := e.pollObject(ctx, s, login, since); err != nil {
					cancel(err)
					return
				}
			}
		})
	}
feed:
	for _, s := range batch {
		select {
		case <-ctx.Done():
			break feed
		case jobs <- s:
		}
	}
	close(jobs)
	wg.Wait()
	return context.Cause(ctx)
}

func (e *extension) pollObject(ctx context.Context, s subscription, login string, since time.Time) error {
	o, err := parseObject(s.Ref)
	if err != nil {
		return err
	}
	d, err := e.detail(ctx, o)
	if api, ok := errors.AsType[*apiError](err); ok && (api.status == 404 || api.status == 410) {
		if s.Closed != nil {
			return nil
		} // 对象消失不能覆盖已证实的合并结论。
		// 只有对象本身的明确 HTTP 终态可以关闭；评论或 CI 端点的 404 不能作对象事实。
		return e.facts(ctx, o, detail{State: "closed", Closed: e.now()}, true)
	}
	if err != nil {
		return err
	}
	if err = e.facts(ctx, o, d, s.Closed != nil); err != nil {
		return err
	}
	if err = e.terminal(ctx, o, d, login, since); err != nil {
		return err
	}
	if o.kind == "pull_request" && d.State == "open" {
		ci, err := e.checks(ctx, o, d.Head.SHA)
		if err != nil {
			return err
		}
		if err = e.ciEvents(ctx, o, ci, login, since); err != nil {
			return err
		}
	}
	// 两种评论分别按 ID 升序读末页，每类至多两个请求。
	for _, kind := range []string{"issues", "pulls"} {
		if kind == "pulls" && o.kind == "issue" {
			break
		}
		rows, _, err := e.recent(ctx, o.path(kind)+"/comments", 1, 0)
		if err != nil {
			return err
		}
		if len(rows) > 0 {
			if err = e.emitComment(ctx, o, rows[len(rows)-1], login, since, "comment", kind); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *extension) facts(ctx context.Context, o object, d detail, reopen bool) error {
	state := d.State
	at := d.Updated
	if state == "closed" {
		at = d.Closed
	}
	if d.Merged != nil && *d.Merged {
		state = "merged"
		at = d.MergedAt
	}
	facts := map[string]any{"state": map[string]string{"value": state}}
	args := map[string]any{"ref": o.ref(), "facts": facts}
	if d.Number > 0 {
		facts["title"] = map[string]string{"value": d.Title}
		facts["urgency"] = map[string]string{"value": "normal"}
		facts["due"] = nil
		if d.Milestone != nil && d.Milestone.DueOn != "" {
			if t, err := time.Parse(time.RFC3339, d.Milestone.DueOn); err == nil {
				facts["due"] = map[string]string{"value": millis(t)}
			}
		}
		args["label"] = d.Title
		if u, err := url.Parse(d.HTML); err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
			args["url"] = d.HTML
		}
	}
	if err := e.call(ctx, "thread/describe", args, nil); err != nil {
		return err
	}
	if state == "open" && !reopen {
		return nil
	}
	if at.IsZero() {
		return errors.New("GitHub 缺少状态变更时间")
	}
	status := map[string]any{"thread": o.ref(), "at": millis(at)}
	if state == "open" {
		return e.call(ctx, "thread/reopened", status, nil)
	}
	status["outcome"] = "closed"
	if state == "merged" {
		status["outcome"] = "done"
	}
	return e.call(ctx, "thread/closed", status, nil)
}

func (e *extension) terminal(ctx context.Context, o object, d detail, login string, since time.Time) error {
	if o.kind != "pull_request" || d.State != "closed" {
		return nil
	}
	event, at, actor := "pr.closed", d.Closed, user{}
	if d.Merged != nil && *d.Merged {
		event, at, actor = "pr.merged", d.MergedAt, d.MergedBy
	}
	if at.Before(since) {
		return nil
	}
	return e.emit(ctx, o, event, millis(at), at, actor, login, "", d.Title, d.State)
}

type status struct {
	ID             int64
	State, Context string
	At             time.Time `json:"updated_at"`
}
type check struct {
	ID                       int64
	Name, Status, Conclusion string
	At                       time.Time `json:"completed_at"`
	App                      struct{ ID int64 }
}
type ciResult struct {
	Combined struct {
		State    string
		Total    *int `json:"total_count"`
		Statuses []status
	}
	Runs struct {
		Total  *int    `json:"total_count"`
		Checks []check `json:"check_runs"`
	}
}

var shaPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}([a-fA-F0-9]{24})?$`)

func (e *extension) checks(ctx context.Context, o object, sha string) (ciResult, error) {
	var ci ciResult
	if !shaPattern.MatchString(sha) {
		return ci, errors.New("GitHub 缺少有效的 head SHA")
	}
	path := "/repos/" + o.repo + "/commits/" + sha
	if _, err := e.get(ctx, path+"/status?per_page=100", &ci.Combined); err != nil {
		return ci, err
	}
	if _, err := e.get(ctx, path+"/check-runs?filter=latest&per_page=100", &ci.Runs); err != nil {
		return ci, err
	}
	if ci.Combined.Total == nil || ci.Runs.Total == nil || ci.Combined.Statuses == nil || ci.Runs.Checks == nil || !slices.Contains([]string{"failure", "pending", "success", "error"}, ci.Combined.State) {
		return ci, errors.New("GitHub 检查结果不完整")
	}
	return ci, nil
}
func (e *extension) ciEvents(ctx context.Context, o object, ci ciResult, login string, since time.Time) error {
	for _, s := range ci.Combined.Statuses {
		if s.ID > 0 && (s.State == "error" || s.State == "failure") && !s.At.Before(since) {
			if err := e.emit(ctx, o, "pr.ci_failed", "status:"+strconv.FormatInt(s.ID, 10), s.At, user{}, login, "", s.Context, s.State); err != nil {
				return err
			}
		}
	}
	for _, c := range ci.Runs.Checks {
		if c.ID > 0 && c.Status == "completed" && !c.At.Before(since) && slices.Contains([]string{"failure", "timed_out", "cancelled", "action_required", "startup_failure"}, c.Conclusion) {
			if err := e.emit(ctx, o, "pr.ci_failed", fmt.Sprintf("check:%d", c.ID), c.At, user{}, login, "", c.Name, c.Conclusion); err != nil {
				return err
			}
		}
	}
	return nil
}
