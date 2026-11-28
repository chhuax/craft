package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/chhuax/craft/pkg/protocol"
)

var markerKey = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,160}$`)

func marker(key string) string { return "<!-- craft:idem:" + key + " -->" }
func denied(reason string) error {
	return &protocol.Error{Code: protocol.ActionDenied, Message: reason}
}

type actionArgs struct {
	Repo                    string `json:"repo"`
	Number                  int    `json:"number"`
	Title, Body, Head, Base string
	WorkID                  string `json:"workId"`
}

func validateAction(p protocol.ActionExecuteParams) (actionArgs, string, string, error) {
	var a actionArgs
	if json.Unmarshal(p.Params, &a) != nil || !validRepo(a.Repo) {
		return a, "", "", invalid("需要有效的 repo（owner/repo）")
	}
	if p.Action != "pr:get" && p.IdempotencyKey == "" {
		return a, "", "", invalid("写动作需要幂等键")
	}
	target := a.Repo + "#" + strconv.Itoa(a.Number)
	var summary string
	switch p.Action {
	case "pr:create":
		if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Base) == "" || strings.TrimSpace(a.Head) == "" {
			return a, "", "", invalid("创建 PR 需要 title、head 和 base")
		}
		target = a.Repo
		summary = fmt.Sprintf("创建 PR %s（%s → %s）\n%s", a.Title, a.Head, a.Base, a.Body)
	case "pr:comment":
		if strings.TrimSpace(a.Body) == "" || strings.Contains(a.Body, "<!-- craft:idem:") || !markerKey.MatchString(p.IdempotencyKey) {
			return a, "", "", invalid("需要有效的评论正文与幂等键")
		}
		summary = "发表评论\n" + a.Body
	case "pr:get":
		summary = "读取 PR"
	case "pr:merge":
		summary = "合并 PR"
	default:
		return a, "", "", invalid("未声明的动作")
	}
	if p.Action != "pr:create" && a.Number < 1 {
		return a, "", "", invalid("需要正整数 number")
	}
	return a, target, summary, nil
}

func (e *extension) execute(ctx context.Context, p protocol.ActionExecuteParams) (any, error) {
	a, target, summary, err := validateAction(p)
	if err != nil {
		return nil, err
	}
	if p.Preview {
		return map[string]any{"action": p.Action, "target": target, "summary": summary, "reversible": p.Action == "pr:comment"}, nil
	}
	o := object{a.Repo, a.Number, "pull_request"}
	var result any
	switch p.Action {
	case "pr:get":
		var d detail
		d, err = e.detail(ctx, o)
		result = map[string]any{"target": target, "title": d.Title, "body": clip(d.Body, 8192), "state": d.State, "author": d.User.Login, "url": d.HTML}
	case "pr:comment":
		result, err = e.postComment(ctx, o, a.Body, p.IdempotencyKey)
	case "pr:create":
		result, err = e.create(ctx, a)
	case "pr:merge":
		result, err = e.merge(ctx, o)
	}
	if api, ok := errors.AsType[*apiError](err); ok {
		switch {
		case p.Action == "pr:merge" && (api.protected || api.status == 405):
			return nil, errors.New("分支保护或合并条件阻止合并")
		case api.status == 401 || api.permission || api.status == 403 && api.retry.IsZero():
			return nil, denied("GitHub 拒绝了当前账号的权限")
		}
	}
	return result, err
}

func (e *extension) create(ctx context.Context, a actionArgs) (any, error) {
	var result struct {
		Number int
		HTML   string `json:"html_url"`
	}
	_, err := e.request(ctx, "POST", "/repos/"+a.Repo+"/pulls", nil, map[string]string{"title": a.Title, "body": a.Body, "head": a.Head, "base": a.Base}, &result)
	if err != nil {
		return nil, err
	}
	if result.Number <= 0 {
		return nil, errors.New("创建 PR 后缺少对象标识")
	}
	ref := object{a.Repo, result.Number, "pull_request"}.ref()
	if a.WorkID != "" {
		if err := e.call(ctx, "work/bindThread", map[string]any{"workId": a.WorkID, "thread": ref, "own": false}, nil); err != nil {
			return nil, fmt.Errorf("PR 已创建，关联工作失败: %w", err)
		}
	}
	return map[string]any{"target": ref.ID, "url": result.HTML}, nil
}

type commentIndex struct {
	Key    string `json:"key"`
	Target string `json:"target"`
	Author int64  `json:"author"`
}

func commentSlot(key string) string {
	// 私有 KV 总共只有 128 个键，已完成回执归内核。槽被覆盖时旧操作降为无法核实。
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("comment.%02x", h[0]%64)
}
func (e *extension) postComment(ctx context.Context, o object, body, key string) (any, error) {
	// issue 的评论端点也接受普通议题，先核对确为 PR。
	if _, err := e.detail(ctx, o); err != nil {
		return nil, err
	}
	account, err := e.account(ctx)
	if err != nil {
		return nil, err
	}
	// 登录名可以转移，只有 GitHub 分配的账号 ID 能跨崩溃核实作者。
	if account.ID <= 0 {
		return nil, denied("无法核实评论账号标识")
	}
	if err = e.save(ctx, commentSlot(key), commentIndex{key, o.ref().ID, account.ID}); err != nil {
		return nil, err
	}
	var receipt struct {
		ID  int64  `json:"id"`
		URL string `json:"html_url"`
	}
	_, err = e.request(ctx, "POST", o.path("issues")+"/comments", nil, map[string]string{"body": body + "\n\n" + marker(key)}, &receipt)
	if err == nil && receipt.ID < 1 {
		err = errors.New("GitHub 评论回执缺少标识")
	}
	return receipt, err
}

func (e *extension) verify(ctx context.Context, p protocol.ActionVerifyParams) (any, error) {
	unknown := map[string]string{"status": "inconclusive"}
	if p.IdempotencyKey == "" {
		return nil, invalid("核实需要幂等键")
	}
	switch p.Action {
	case "pr:create", "pr:merge", "pr:get":
		return unknown, nil
	case "pr:comment":
	default:
		return nil, invalid("未知动作")
	}
	if !markerKey.MatchString(p.IdempotencyKey) {
		return nil, invalid("评论幂等键无效")
	}
	var idx commentIndex
	if err := e.state(ctx, commentSlot(p.IdempotencyKey), &idx); err != nil {
		return nil, err
	}
	if idx.Key != p.IdempotencyKey || idx.Author <= 0 {
		return unknown, nil
	}
	o, err := parseObject(protocol.ThreadRef{Source: "github", Kind: "pull_request", ID: idx.Target})
	if err != nil {
		return nil, err
	}
	for page := 1; page != 0; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var rows []comment
		h, err := e.get(ctx, fmt.Sprintf("%s/comments?per_page=100&page=%d", o.path("issues"), page), &rows)
		if err != nil {
			return nil, err
		}
		for _, c := range rows {
			if c.ID > 0 && c.User.ID == idx.Author && strings.HasSuffix(c.Body, "\n\n"+marker(idx.Key)) {
				return map[string]any{"status": "applied", "receipt": map[string]any{"id": c.ID, "target": idx.Target}}, nil
			}
		}
		more, err := linkPage(h, "next")
		if err != nil || more != 0 && more <= page {
			return nil, errors.New("核实评论分页无效")
		}
		page = more
	}
	// 评论可以被编辑或删除，完整列表里找不到标记也不能证明从未发出。
	return unknown, nil
}

func (e *extension) merge(ctx context.Context, o object) (any, error) {
	current, err := e.account(ctx)
	if err != nil {
		return nil, denied("无法核实当前登录名")
	}
	d, err := e.detail(ctx, o)
	if err != nil {
		return nil, denied("无法核实 PR 作者或状态")
	}
	// 两次查询之间登录名可能变更；有稳定账号标识时也要核对。
	if d.User.Login == "" || strings.EqualFold(d.User.Login, current.Login) || current.ID > 0 && d.User.ID == current.ID {
		return nil, denied("需要核实 PR 由其他账号提出")
	}
	if d.State != "open" || d.Draft || d.Mergeable == nil || !*d.Mergeable || (d.MergeState != "clean" && d.MergeState != "unstable") {
		return nil, errors.New("分支保护、冲突或 PR 状态阻止合并")
	}
	ci, err := e.checks(ctx, o, d.Head.SHA)
	if err != nil {
		return nil, err
	}
	if err = e.requiredChecks(ctx, o, d.Base.Ref, ci); err != nil {
		return nil, err
	}
	var receipt struct {
		Merged bool   `json:"merged"`
		SHA    string `json:"sha"`
	}
	// 即使预检后被推送新提交，GitHub 也只会合并刚检查过的 head。
	_, err = e.request(ctx, "PUT", o.endpoint()+"/merge", nil, map[string]string{"sha": d.Head.SHA}, &receipt)
	if err == nil && !receipt.Merged {
		err = errors.New("GitHub 未确认合并成功")
	}
	return receipt, err
}

type requirement struct {
	Context     string
	App         int64 `json:"app_id"`
	Integration int64 `json:"integration_id"`
}

func (ci ciResult) passed(r requirement) bool {
	found := false
	app := max(r.App, r.Integration)
	for _, s := range ci.Combined.Statuses {
		if s.Context == r.Context {
			if s.State != "success" {
				return false
			}
			if app <= 0 {
				found = true
			}
		}
	}
	for _, c := range ci.Runs.Checks {
		if c.Name == r.Context && (app <= 0 || c.App.ID == app) {
			if c.Status != "completed" || !slices.Contains([]string{"success", "neutral", "skipped"}, c.Conclusion) {
				return false
			}
			found = true
		}
	}
	return r.Context != "" && found
}
func (e *extension) requiredChecks(ctx context.Context, o object, base string, ci ciResult) error {
	if base == "" || base == "." || base == ".." {
		return errors.New("不能核实目标分支")
	}
	if *ci.Combined.Total > len(ci.Combined.Statuses) || *ci.Runs.Total > len(ci.Runs.Checks) {
		return errors.New("无法完整核实当前提交的检查")
	}
	path := "/repos/" + o.repo
	var branch struct {
		Name       string
		Protected  *bool
		Protection *struct {
			Required struct {
				Contexts []string
				Checks   []requirement
			} `json:"required_status_checks"`
		}
	}
	_, err := e.get(ctx, path+"/branches/"+url.PathEscape(base), &branch)
	if err != nil {
		return err
	}
	if branch.Name != base || branch.Protected == nil || *branch.Protected && branch.Protection == nil {
		return errors.New("无法核实目标分支保护")
	}
	var required []requirement
	if branch.Protection != nil {
		required = branch.Protection.Required.Checks
		for _, name := range branch.Protection.Required.Contexts {
			required = append(required, requirement{Context: name})
		}
	}
	var rules []struct {
		Type       string
		Parameters struct {
			Required []requirement `json:"required_status_checks"`
		}
	}
	h, err := e.get(ctx, path+"/rules/branches/"+url.PathEscape(base)+"?per_page=100", &rules)
	if err != nil {
		return err
	}
	if next, err := linkPage(h, "next"); err != nil || next != 0 {
		return errors.New("分支规则未读取完整")
	}
	for _, r := range rules {
		if r.Type == "required_status_checks" {
			if r.Parameters.Required == nil {
				return errors.New("分支规则缺少必需检查")
			}
			required = append(required, r.Parameters.Required...)
		}
	}
	for _, r := range required {
		if !ci.passed(r) {
			return errors.New("目标分支的必需检查缺失或未通过")
		}
	}
	return nil
}
