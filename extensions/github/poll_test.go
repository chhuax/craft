package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

const repo = "north/demo"
const sha = "0123456789abcdef0123456789abcdef01234567"

func pageNumber(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("page"))
	return max(1, n)
}
func nextLink(w http.ResponseWriter, n int) {
	w.Header().Set("Link", fmt.Sprintf(`<https://api.github.com/notifications?page=%d>; rel="next"`, n))
}
func sendJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
func notice(reason, kind string, n int) map[string]any {
	segment := "issues"
	if kind == "PullRequest" {
		segment = "pulls"
	}
	base := "https://api.github.com/repos/" + repo
	return map[string]any{"id": strconv.Itoa(n), "updated_at": instant, "reason": reason, "repository": map[string]string{"full_name": repo}, "subject": map[string]string{"type": kind, "url": fmt.Sprintf("%s/%s/%d", base, segment, n), "latest_comment_url": fmt.Sprintf("%s/issues/comments/%d", base, n), "title": "例子"}}
}
func writeComment(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
	sendJSON(w, map[string]any{"id": n, "created_at": instant, "user": user{Login: "author", Type: "User", ID: 21}, "body": "@reader 请看"})
}
func pr(n int, state, author string) map[string]any {
	return map[string]any{"number": n, "state": state, "title": "变更", "body": "原文", "updated_at": instant, "merged": false, "user": map[string]string{"login": author, "type": "User"}, "head": map[string]string{"sha": sha}, "base": map[string]string{"ref": "main"}, "mergeable": true, "mergeable_state": "clean"}
}
func sub(n int, kind string) subscription { return subscription{Ref: object{repo, n, kind}.ref()} }

func TestInboxPagesAcknowledgementAndValidators(t *testing.T) {
	var mode atomic.Int32
	var pages atomic.Int32
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("请求缺少凭据")
		}
		switch r.URL.Path {
		case "/notifications":
			pages.Add(1)
			q := r.URL.Query()
			since, err := time.Parse(time.RFC3339Nano, q.Get("since"))
			if err != nil || since.Before(instant.AddDate(0, 0, -7)) || q.Get("participating") != "true" {
				t.Error("回看窗口无效")
			}
			if mode.Load() == 2 {
				if r.Header.Get("If-None-Match") != `"v1"` || r.Header.Get("If-Modified-Since") != instant.Format(http.TimeFormat) {
					t.Error("缺少条件请求头")
				}
				w.WriteHeader(304)
				return
			}
			if pageNumber(r) == 1 {
				nextLink(w, 2)
				w.Header().Set("ETag", `"v1"`)
				w.Header().Set("Last-Modified", instant.Format(http.TimeFormat))
				w.Header().Set("X-Poll-Interval", "60")
				sendJSON(w, []any{notice("review_requested", "PullRequest", 11)})
				return
			}
			if mode.Load() == 0 {
				http.Error(w, secret, 500)
				return
			}
			sendJSON(w, []any{notice("mention", "Issue", 12)})
		case "/user":
			sendJSON(w, map[string]string{"login": "reader"})
		case "/repos/north/demo/issues/11/events":
			sendJSON(w, []any{map[string]any{"id": 55, "event": "review_requested", "actor": user{Login: "sam", Type: "User", ID: 21}, "requested_reviewer": user{Login: "reader", Type: "User", ID: 17}, "created_at": instant}})
		case "/repos/north/demo/issues/comments/12":
			writeComment(w, r)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	if h.run("inbox") == nil || !h.health("error") || len(h.triggers()) != 0 || h.state["cursor"] != nil {
		t.Fatal("分页失败仍投递或提交")
	}
	mode.Store(1)
	h.clock.Add(60000)
	h.reject = "watch/receive"
	if h.run("inbox") == nil || h.state["cursor"] != nil {
		t.Fatal("拒收后提交游标")
	}
	h.reject = ""
	h.clock.Add(60000)
	must(t, h.run("inbox"))
	ts := h.triggers()
	want := []string{"state/get", "state/get", "state/get", "state/set", "watch/receive", "watch/receive", "state/set", "health/report"}
	if !slices.Equal(h.methods(), want) || len(ts) != 2 {
		t.Fatal(h.methods())
	}
	for i, tr := range ts {
		ref := []protocol.ThreadRef{sub(11, "pull_request").Ref, sub(12, "issue").Ref}[i]
		if len(tr.ID) != 32 || !tr.Human || tr.Source != "github" || !slices.Equal(tr.Threads, []protocol.ThreadRef{ref}) || tr.Addressed == nil || *tr.Addressed != []string{"direct", "mention"}[i] || tr.Event != []string{"pr.review_requested", "issue.comment"}[i] {
			t.Fatal(tr)
		}
	}
	before := pages.Load()
	must(t, h.run("inbox"))
	if pages.Load() != before || len(h.triggers()) != 0 {
		t.Fatal("提前轮询")
	}
	h.clock.Add(60000)
	mode.Store(2)
	must(t, h.run("inbox"))
	if pages.Load() != before+1 || len(h.triggers()) != 0 {
		t.Fatal("304 产生事件")
	}
	mode.Store(1)
	delete(h.state, "cursor")
	must(t, h.run("inbox"))
	for i, tr := range h.triggers() {
		if tr.ID != ts[i].ID {
			t.Fatal("重放改变事件标识")
		}
	}
}

func TestFailuresAndCredentials(t *testing.T) {
	for _, code := range []int{401, 403, 429, 500} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			var hits atomic.Int32
			h := start(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if code == 403 {
					w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(instant.Unix()+120, 10))
				}
				if code == 429 {
					w.Header().Set("Retry-After", "120")
				}
				http.Error(w, secret, code)
			})
			t.Setenv(tokenVar, "")
			must(t, h.run("inbox"))
			if hits.Load() != 0 || !h.health("paused") {
				t.Fatal("缺凭据未暂停")
			}
			t.Setenv(tokenVar, secret)
			err := h.run("inbox")
			if (code == 500) != (err != nil) || code == 500 && !h.health("error") || code != 500 && !h.health("paused") {
				t.Fatal(err, h.methods())
			}
			if code == 403 || code == 429 {
				if string(h.state["blocked_until"]) != strconv.FormatInt(instant.Add(2*time.Minute).UnixMilli(), 10) {
					t.Fatal("限流时间未记录")
				}
				must(t, h.run("subscribed"))
				if hits.Load() != 1 {
					t.Fatal("限流中访问了 GitHub")
				}
				h.clock.Add(120000)
				must(t, h.run("subscribed"))
				if hits.Load() != 2 {
					t.Fatal("限流到期仍未恢复")
				}
			}
		})
	}
}

func TestSubscribedFactsEchoAndAbsence(t *testing.T) {
	for _, mode := range []string{"self", "merged", "empty", "null", "blank", "404", "410", "comment404", "token", "reopened"} {
		t.Run(mode, func(t *testing.T) {
			h := start(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/user":
					sendJSON(w, user{Login: "reader", Type: "User", ID: 17})
				case "/repos/north/demo/pulls/5":
					switch mode {
					case "empty":
						sendJSON(w, map[string]any{})
					case "blank":
						return
					case "null":
						sendJSON(w, nil)
					case "404", "410":
						code, _ := strconv.Atoi(mode)
						w.WriteHeader(code)
					case "token":
						_, _ = fmt.Fprint(w, `{"number":5,"state":"open","body":"\u0067hp_replay_only_194267"}`)
					default:
						d := pr(5, "closed", "author")
						if mode == "reopened" {
							d["state"] = "open"
						} else {
							d["merged"] = true
							d["merged_at"] = instant
							actor := "other"
							if mode == "self" {
								actor = "Reader"
							}
							d["merged_by"] = user{Login: actor, Type: "User", ID: 21}
						}
						sendJSON(w, d)
					}
				default:
					switch {
					case strings.HasSuffix(r.URL.Path, "/comments"):
						if mode == "comment404" {
							w.WriteHeader(404)
							return
						}
						sendJSON(w, []any{})
					case strings.HasSuffix(r.URL.Path, "/check-runs"):
						sendJSON(w, map[string]any{"total_count": 0, "check_runs": []any{}})
					case strings.HasSuffix(r.URL.Path, "/status"):
						sendJSON(w, map[string]any{"state": "failure", "total_count": 1, "statuses": []any{map[string]any{"context": "build", "state": "failure", "id": 99, "updated_at": instant}}})
					default:
						t.Error(r.URL.Path)
						w.WriteHeader(404)
					}
				}
			})
			s := sub(5, "pull_request")
			if mode == "reopened" {
				s.Closed = &struct {
					At string `json:"at"`
				}{At: millis(instant.Add(-time.Hour))}
			}
			h.subs = []subscription{s}
			err := h.run("subscribed")
			methods := h.methods()
			describe, closed, receive, save := slices.Index(methods, "thread/describe"), slices.Index(methods, "thread/closed"), slices.Index(methods, "watch/receive"), slices.Index(methods, "state/set")
			switch mode {
			case "null", "empty", "blank", "token":
				if err == nil || !h.health("error") || closed >= 0 || save >= 0 || receive >= 0 {
					t.Fatal(methods)
				}
			case "reopened":
				reopen := slices.Index(methods, "thread/reopened")
				if err != nil || describe < 0 || reopen <= describe || receive <= reopen || save <= receive || h.triggers()[0].Human || h.triggers()[0].Event != "pr.ci_failed" {
					t.Fatal(methods, err)
				}
			default:
				if describe < 0 || closed <= describe {
					t.Fatal(methods)
				}
				if mode == "comment404" {
					if err == nil || save >= 0 || slices.Index(methods[closed+1:], "thread/closed") >= 0 {
						t.Fatal(methods)
					}
					return
				}
				must(t, err)
				if mode == "merged" {
					if receive <= closed || save <= receive || !h.triggers()[0].Human || h.triggers()[0].Event != "pr.merged" {
						t.Fatal(methods)
					}
				} else if receive >= 0 {
					t.Fatal("回声或消失对象被投递")
				}
				var facts struct {
					Facts map[string]struct{ Value string }
				}
				must(t, json.Unmarshal(h.calls[describe].Params, &facts))
				if facts.Facts["state"].Value == "" {
					t.Fatal("状态事实无值")
				}
			}
		})
	}
}

func TestBatchLimitsAndContinuation(t *testing.T) {
	var active, total, peak atomic.Int32
	gate := make(chan struct{})
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		total.Add(1)
		switch {
		case r.URL.Path == "/user":
			sendJSON(w, user{Login: "reader", Type: "User", ID: 17})
		case strings.HasSuffix(r.URL.Path, "/status"):
			sendJSON(w, map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			sendJSON(w, map[string]any{"total_count": 0, "check_runs": []any{}})
		case strings.HasSuffix(r.URL.Path, "/comments"):
			if pageNumber(r) == 1 {
				w.Header().Set("Link", `<?page=20>; rel="last"`)
			}
			sendJSON(w, []any{})
		default:
			n := active.Add(1)
			defer active.Add(-1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			if total.Load() <= 5 {
				if n == 4 {
					close(gate)
				}
				select {
				case <-gate:
				case <-r.Context().Done():
					return
				}
			}
			var id int
			_, err := fmt.Sscanf(r.URL.Path, "/repos/north/demo/pulls/%d", &id)
			if err != nil {
				t.Error(err)
			}
			sendJSON(w, pr(id, "open", "author"))
		}
	})
	for i := range 101 {
		h.subs = append(h.subs, sub(i+1, "pull_request"))
	}
	must(t, h.run("subscribed"))
	if total.Load() != 701 || peak.Load() != 4 {
		t.Fatalf("请求 %d，并发 %d", total.Load(), peak.Load())
	}
	visited := map[string]bool{}
	count := func() {
		for _, c := range h.calls {
			if c.Method == "thread/describe" {
				var p struct{ Ref protocol.ThreadRef }
				must(t, json.Unmarshal(c.Params, &p))
				visited[p.Ref.ID] = true
			}
		}
	}
	count()
	must(t, h.run("subscribed"))
	count()
	if len(visited) != 101 || total.Load() != 709 {
		t.Fatal(len(visited), total.Load())
	}
}

func TestCheckSuiteAndSubscriptionIdentifySameFailure(t *testing.T) {
	h := start(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/user":
			sendJSON(w, user{Login: "reader", ID: 17, Type: "User"})
		case "/notifications":
			n := notice("ci_activity", "CheckSuite", 5)
			n["subject"] = map[string]string{"type": "CheckSuite", "url": "https://api.github.com/repos/north/demo/check-suites/42"}
			sendJSON(w, []any{n})
		case "/repos/north/demo/check-suites/42":
			sendJSON(w, map[string]any{"id": 42, "head_sha": sha, "pull_requests": []any{map[string]any{"number": 5, "url": "https://api.github.com/repos/north/demo/pulls/5"}}})
		case "/repos/north/demo/pulls/5":
			sendJSON(w, pr(5, "open", "author"))
		case "/repos/north/demo/commits/" + sha + "/status":
			sendJSON(w, map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
		case "/repos/north/demo/commits/" + sha + "/check-runs":
			sendJSON(w, map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"id": 47, "name": "build", "status": "completed", "conclusion": "failure", "completed_at": instant}}})
		case "/repos/north/demo/issues/5/comments", "/repos/north/demo/pulls/5/comments":
			sendJSON(w, []any{})
		default:
			t.Error(r.URL.Path)
			w.WriteHeader(404)
		}
	})
	h.subs = []subscription{sub(5, "pull_request")}
	must(t, h.run("inbox"))
	inbox := h.triggers()
	must(t, h.run("subscribed"))
	follow := h.triggers()
	if len(inbox) != 1 || len(follow) != 1 || inbox[0].ID != follow[0].ID || inbox[0].Human || inbox[0].Event != "pr.ci_failed" {
		t.Fatal(inbox, follow)
	}
}
