package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

func args(action string) map[string]any {
	return map[string]any{"action": action, "preview": false, "idempotencyKey": "attempt-1", "params": map[string]any{"repo": repo, "number": 5, "title": "变更", "head": "feature", "base": "main", "body": "评审意见", "workId": "w1"}}
}

func TestActionsThroughRPC(t *testing.T) {
	var writes, reads atomic.Int32
	var mu sync.Mutex
	var body string
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			reads.Add(1)
		} else {
			writes.Add(1)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /user":
			sendJSON(w, user{Login: "reader", Type: "User", ID: 17})
		case "GET /repos/north/demo/pulls/5":
			sendJSON(w, pr(5, "open", "author"))
		case "GET /repos/north/demo/commits/" + sha + "/status":
			sendJSON(w, map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
		case "GET /repos/north/demo/commits/" + sha + "/check-runs":
			sendJSON(w, map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"id": 42, "name": "build", "status": "completed", "conclusion": "success", "app": map[string]int{"id": 3}}}})
		case "GET /repos/north/demo/branches/main":
			sendJSON(w, map[string]any{"name": "main", "protected": true, "protection": map[string]any{"required_status_checks": map[string]any{"contexts": []string{"build"}}}})
		case "GET /repos/north/demo/rules/branches/main":
			_, _ = fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":3}]}}]`)
		case "POST /repos/north/demo/issues/5/comments":
			var p struct{ Body string }
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Error(err)
			}
			if !strings.HasSuffix(p.Body, "\n\n<!-- craft:idem:attempt-1 -->") {
				t.Error("缺少幂等标记")
			}
			mu.Lock()
			body = p.Body
			mu.Unlock()
			sendJSON(w, map[string]any{"id": 77, "html_url": "https://github.com/north/demo/pull/5#issuecomment-77"})
		case "GET /repos/north/demo/issues/5/comments":
			mu.Lock()
			defer mu.Unlock()
			if pageNumber(r) == 1 {
				nextLink(w, 2)
				sendJSON(w, []any{map[string]any{"id": 11, "user": user{Login: "someone", Type: "User", ID: 21}, "body": body}})
				return
			}
			sendJSON(w, []any{map[string]any{"id": 77, "user": user{Login: "reader", Type: "User", ID: 17}, "body": body}})
		case "PUT /repos/north/demo/pulls/5/merge":
			var p struct{ SHA string }
			if json.NewDecoder(r.Body).Decode(&p) != nil || p.SHA != sha {
				t.Error("合并未锁定已核实的 head")
			}
			sendJSON(w, map[string]any{"merged": true, "sha": sha})
		case "POST /repos/north/demo/pulls":
			var p map[string]string
			if json.NewDecoder(r.Body).Decode(&p) != nil || p["base"] != "main" || p["head"] != "feature" || len(p) != 4 {
				t.Error("创建参数无效")
			}
			sendJSON(w, map[string]any{"number": 9, "html_url": "https://github.com/north/demo/pull/9"})
		default:
			t.Error(r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	for _, action := range []string{"pr:get", "pr:create", "pr:merge", "pr:comment"} {
		p := args(action)
		p["preview"] = true
		var preview struct {
			Summary, Target string
			Reversible      bool
		}
		requests := reads.Load() + writes.Load()
		must(t, json.Unmarshal(h.ok("action/execute", p), &preview))
		target := repo + "#5"
		if action == "pr:create" {
			target = repo
		}
		if preview.Summary == "" || preview.Target != target || preview.Reversible != (action == "pr:comment") || reads.Load()+writes.Load() != requests {
			t.Fatal(preview)
		}
		p["preview"] = false
		h.ok("action/execute", p)
		if action == "pr:create" && !slices.Contains(h.methods(), "work/bindThread") {
			t.Fatal("创建后未绑定")
		}
		var v struct {
			Status  string
			Receipt struct{ ID int }
		}
		must(t, json.Unmarshal(h.ok("action/verify", map[string]string{"action": action, "idempotencyKey": "attempt-1"}), &v))
		if action == "pr:comment" {
			if v.Status != "applied" || v.Receipt.ID != 77 {
				t.Fatal("误信复制标记的评论", v)
			}
		} else if v.Status != "inconclusive" {
			t.Fatal(v)
		}
	}
	if writes.Load() != 3 {
		t.Fatal(writes.Load())
	}
	for _, action := range []string{"pr:get", "pr:create", "pr:merge", "pr:comment"} {
		p := args(action)
		p["params"] = map[string]string{"repo": "n/.."}
		_, err := h.invoke("action/execute", p)
		if e, ok := errors.AsType[*protocol.Error](err); !ok || e.Code != -32602 {
			t.Fatal(action, err)
		}
	}
}

func TestIncompleteWriteRequest(t *testing.T) {
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("无效的写请求访问了 %s", r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	})
	for _, field := range []string{"preview", "idempotencyKey"} {
		for _, missing := range []bool{true, false} {
			p := args("pr:create")
			p[field] = nil
			if missing {
				delete(p, field)
			}
			_, err := h.invoke("action/execute", p)
			if rpc, ok := errors.AsType[*protocol.Error](err); !ok || rpc.Code != -32602 {
				t.Fatal(field, err)
			}
		}
	}
}

func TestMergeFailsClosed(t *testing.T) {
	for _, mode := range []string{"self", "renamedSelf", "noAuthor", "noUser", "closed", "blocked", "missingProtection", "requiredMissing", "wrongApp", "checkFailed", "statusPending", "forbidden", "protected", "headChanged"} {
		t.Run(mode, func(t *testing.T) {
			var writes atomic.Int32
			h := start(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/user":
					if mode == "noUser" {
						sendJSON(w, map[string]any{})
						return
					}
					sendJSON(w, user{Login: "reader", Type: "User", ID: 17})
				case r.Method == "PUT":
					writes.Add(1)
					code := 403
					msg := "permission denied"
					switch mode {
					case "protected":
						code = 405
						msg = "protected branch"
					case "headChanged":
						code = 409
					}
					http.Error(w, msg, code)
				case strings.HasSuffix(r.URL.Path, "/pulls/5"):
					d := pr(5, "open", "author")
					switch mode {
					case "self":
						d["user"] = user{Login: "READER", Type: "User", ID: 21}
					case "renamedSelf":
						d["user"] = user{Login: "renamed", Type: "User", ID: 17}
					case "noAuthor":
						delete(d, "user")
					case "closed":
						d["state"] = "closed"
					case "blocked":
						d["mergeable_state"] = "blocked"
					}
					sendJSON(w, d)
				case strings.HasSuffix(r.URL.Path, "/status"):
					statuses := []any{}
					if mode == "statusPending" {
						statuses = append(statuses, map[string]string{"state": "pending", "context": "build"})
					}
					sendJSON(w, map[string]any{"total_count": len(statuses), "state": "pending", "statuses": statuses})
				case strings.HasSuffix(r.URL.Path, "/check-runs"):
					checks := []any{}
					if mode != "requiredMissing" {
						app := 3
						conclusion := "success"
						if mode == "wrongApp" {
							app = 4
						}
						if mode == "checkFailed" {
							conclusion = "failure"
						}
						checks = append(checks, map[string]any{"id": 7, "name": "build", "status": "completed", "conclusion": conclusion, "app": map[string]int{"id": app}})
					}
					sendJSON(w, map[string]any{"total_count": len(checks), "check_runs": checks})
				case strings.Contains(r.URL.Path, "/rules/"):
					_, _ = fmt.Fprint(w, `[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"build","integration_id":3}]}}]`)
				case strings.HasSuffix(r.URL.Path, "/branches/main"):
					branch := map[string]any{"name": "main", "protected": true}
					if mode != "missingProtection" {
						branch["protection"] = map[string]any{"enabled": true, "required_status_checks": map[string]any{"contexts": []string{}}}
					}
					sendJSON(w, branch)
				default:
					t.Error(r.URL.Path)
					w.WriteHeader(404)
				}
			})
			_, err := h.invoke("action/execute", args("pr:merge"))
			rpc, ok := errors.AsType[*protocol.Error](err)
			if !ok {
				t.Fatal("允许了合并", mode)
			}
			wantDenied := slices.Contains([]string{"self", "renamedSelf", "noAuthor", "noUser", "forbidden"}, mode)
			if (rpc.Code == protocol.ActionDenied) != wantDenied {
				t.Fatal(rpc)
			}
			attempted := slices.Contains([]string{"forbidden", "protected", "headChanged"}, mode)
			if (writes.Load() == 1) != attempted {
				t.Fatal("合并写入次数", writes.Load())
			}
		})
	}
}

func TestContextReadsRecentPages(t *testing.T) {
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/pulls/5") {
			sendJSON(w, pr(5, "open", "author"))
			return
		}
		if pageNumber(r) == 1 {
			w.Header().Set("Link", `<?page=2>; rel="next", <?page=5>; rel="last"`)
		}
		sendJSON(w, []any{map[string]any{"id": pageNumber(r), "body": strings.Repeat("甲", 3000), "submitted_at": instant, "user": user{Login: "sam", Type: "User", ID: 21}}})
	})
	p := map[string]any{"thread": sub(5, "pull_request").Ref}
	var ctx struct {
		Body                              string
		Comments, Reviews, ReviewComments []struct {
			ID   int
			Body string
		}
		Cursor map[string]int
	}
	must(t, json.Unmarshal(h.ok("context/read", p), &ctx))
	if ctx.Body != "原文" || len(ctx.Comments) != 1 || len(ctx.Reviews) != 1 || len(ctx.ReviewComments) != 1 || len(ctx.Comments[0].Body) > 4096 || ctx.Comments[0].ID != 5 || ctx.Cursor["reviews"] != 4 {
		t.Fatal("上下文分页或正文上限错误")
	}
	p["cursor"] = ctx.Cursor
	must(t, json.Unmarshal(h.ok("context/read", p), &ctx))
	if ctx.Comments[0].ID != 4 || ctx.Cursor["reviews"] != 3 {
		t.Fatal("上下文续页错误")
	}
}

func TestLiveGitHubReadOnly(t *testing.T) {
	if os.Getenv("CRAFT_E2E_GITHUB") != "1" || os.Getenv("GITHUB_TOKEN") == "" {
		t.Skip("需要显式启用与只读令牌")
	}
	cfg := protocol.InitializeParams{Protocol: 1, Config: json.RawMessage(`{}`), StateDir: t.TempDir()}
	e, err := initialize(raw(t, cfg), nil, time.Now)
	must(t, err)
	_, err = e.me(t.Context())
	must(t, err)
	_, err = e.detail(t.Context(), object{"cli/cli", 14696, "pull_request"})
	must(t, err)
}

func TestAssignedBotsAndEcho(t *testing.T) {
	var actor atomic.Int32
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			sendJSON(w, user{Login: "reader", Type: "User", ID: 17})
		case "/notifications":
			sendJSON(w, []any{notice("assign", "Issue", 3)})
		default:
			actors := []user{{Login: "reader", Type: "User"}, {Login: "service", Type: "Bot", ID: 30}, {Login: "service[bot]", Type: "User", ID: 31}, {Login: "person", Type: "User", ID: 21}}
			sendJSON(w, []any{map[string]any{"id": 49, "event": "assigned", "created_at": instant, "assignee": user{Login: "reader", Type: "User", ID: 17}, "actor": actors[actor.Load()]}, map[string]any{"id": 50, "event": "assigned", "created_at": instant, "assignee": user{Login: "else", Type: "User", ID: 21}, "actor": user{Login: "sam", Type: "User", ID: 21}}})
		}
	})
	for i := range 4 {
		actor.Store(int32(i))
		must(t, h.run("inbox"))
		ts := h.triggers()
		if i == 0 {
			if len(ts) != 0 {
				t.Fatal("自己的指派回声")
			}
			continue
		}
		if len(ts) != 1 || ts[0].Event != "issue.assigned" || ts[0].Human != (i == 3) || ts[0].Addressed == nil || *ts[0].Addressed != "direct" {
			t.Fatal(ts)
		}
	}
}
