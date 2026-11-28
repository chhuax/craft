package codex

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/host"
)

type fixtureThreads struct {
	dir, title string
	working    bool
	loaded     bool
}

var errDisconnect = errors.New("模拟丢失回执")

func (f *fixtureThreads) handle(method string, raw json.RawMessage) (any, error) {
	var p struct {
		Env                                                                          map[string]string
		Cwd, Sandbox, ApprovalPolicy, ThreadID, Name, TurnID, ExpectedTurnID, Cursor string
		IncludeTurns                                                                 bool
		Input                                                                        []struct{ Type, Text string }
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	id := "thread-a"
	status := "idle"
	if f.working {
		status = "active"
	}
	th := map[string]any{"id": id, "status": map[string]string{"type": status}, "name": f.title, "cwd": f.dir}
	switch method {
	case "thread/start":
		if _, err := os.Stat(filepath.Join(f.dir, "lose-create-response")); err == nil {
			return nil, errDisconnect
		}
		if p.Cwd != f.dir || p.Sandbox != "read-only" || p.ApprovalPolicy != "never" || os.Getenv(host.EnvHost) != "codex" {
			return nil, errors.New("线程配置无效")
		}
		if os.Getenv(host.EnvWork) != "" || os.Getenv(host.EnvAssignment) != "" || os.Getenv(host.EnvCodexThreadID) != "" {
			return nil, errors.New("共享服务继承了线程归属")
		}
		if p.Env[host.EnvWork] != "" {
			if len(p.Env) != 4 || p.Env[host.EnvAssignment] != "a-"+p.Env[host.EnvWork] || p.Env[host.EnvHost] != hostName || p.Env[host.EnvRole] != "worker" {
				return nil, errors.New("线程绑定错误")
			}
			th["id"] = p.Env[host.EnvWork]
		}
		f.loaded = true
		return map[string]any{"thread": th}, nil
	case "thread/name/set":
		f.title = p.Name
		return struct{}{}, nil
	case "thread/list":
		if p.Cursor == "" {
			return map[string]any{"data": []any{}, "nextCursor": "more"}, nil
		}
		return map[string]any{"data": []any{th}, "nextCursor": nil}, nil
	case "thread/resume", "thread/read":
		switch p.ThreadID {
		case "conflict":
			return nil, errors.New("already has an active writer")
		case "missing":
			return nil, errors.New("thread not loaded")
		}
		if method == "thread/resume" {
			f.loaded = true
		}
		if p.IncludeTurns && f.working {
			th["turns"] = []turn{{ID: "turn-a", Status: "inProgress"}}
		}
		return map[string]any{"thread": th}, nil
	case "turn/start", "turn/steer":
		if !f.loaded {
			return nil, errors.New("thread not loaded")
		}
		if len(p.Input) != 1 || p.Input[0].Type != "text" || !strings.HasPrefix(p.Input[0].Text, "[craft:delivery:") {
			return nil, errors.New("未收到标记")
		}
		if strings.HasSuffix(p.Input[0].Text, "丢失回执") {
			return nil, errDisconnect
		}
		if f.working && (method != "turn/steer" || p.ExpectedTurnID != "turn-a") {
			return nil, errors.New("忙时续轮参数错误")
		}
		f.working = true
		if method == "turn/steer" {
			return map[string]string{"turnId": "turn-a"}, nil
		}
		return map[string]any{"turn": map[string]string{"id": "turn-a"}}, nil
	case "turn/interrupt":
		if p.TurnID != "turn-a" || !f.working {
			return nil, errors.New("中断了错误轮次")
		}
		f.working = false
		return struct{}{}, nil
	default:
		return nil, fmt.Errorf("未知方法 %s", method)
	}
}

func settings(t testing.TB) (*Driver, host.CreateSpec) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("HOME", dir)
	d := New(os.Args[0])
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d, host.CreateSpec{Directory: dir, Title: "短标题", Permissions: host.Permissions{WriteScope: host.ReadOnly}}
}

func TestThreadLifecycle(t *testing.T) {
	d, spec := settings(t)
	var registered host.Session
	spec.OnCreated = func(s host.Session) error { registered = s; return nil }
	s, err := d.Create(t.Context(), spec)
	if err != nil || s != registered || s.ID == "" {
		t.Fatal(s, registered, err)
	}
	matches, err := d.FindByTitle(t.Context(), spec.Directory, spec.Title)
	if err != nil || len(matches) != 1 || matches[0] != s {
		t.Fatal(matches, err)
	}
	for _, id := range []string{"start", "steer"} {
		if r := d.Append(t.Context(), s, "文本", id); r.Outcome != host.OK {
			t.Fatal(r)
		}
	}
	if d.Status(t.Context(), s) != host.Busy {
		t.Fatal("漏掉运行中的轮次")
	}
	if err := d.Stop(t.Context(), s, time.Second*10); err != nil {
		t.Fatal(err)
	}
	if d.Status(t.Context(), s) != host.Idle {
		t.Fatal("中断后仍忙")
	}
	for _, id := range []string{"conflict", "missing"} {
		other := host.Session{Host: hostName, ID: id}
		if d.Status(t.Context(), other) != host.UnknownState || d.Append(t.Context(), other, "文本", "id").Outcome != host.Later {
			t.Fatal(id)
		}
	}
	if r := d.Append(t.Context(), s, "丢失回执", "lost"); r.Outcome != host.Unknown {
		t.Fatal(r)
	}
}

func TestFreshDriverResumes(t *testing.T) {
	d, _ := settings(t)
	s := host.Session{Host: hostName, ID: "thread-a"}
	if r := d.Append(t.Context(), s, "续轮", "fresh"); r.Outcome != host.OK {
		t.Fatal(r)
	}
}

func TestThreadRegistrationFails(t *testing.T) {
	d, spec := settings(t)
	spec.DeliveryID = "invalid/id"
	if _, err := d.Create(t.Context(), spec); !errors.Is(err, host.Unsupported) {
		t.Fatal(err)
	}
	spec.Prompt, spec.DeliveryID = "正文", "valid"
	spec.OnCreated = func(host.Session) error { return errors.New("代次已变") }
	s, err := d.Create(t.Context(), spec)
	if s.ID == "" || !errors.Is(err, host.Unknown) || d.Status(t.Context(), s) != host.Idle {
		t.Fatal(s, err)
	}
}

func TestPermissionsAndStatus(t *testing.T) {
	for _, p := range []host.Permissions{{WriteScope: host.Worktree}, {NoNetwork: true}, {Tools: []string{}}} {
		if _, err := sandbox(p); !errors.Is(err, host.Unsupported) {
			t.Fatal(p, err)
		}
	}
	for _, tc := range []struct {
		raw  string
		want host.State
	}{{"waiting", host.Busy}, {"idle", host.Idle}, {"archived", host.Gone}, {"notLoaded", host.UnknownState}, {"other", host.UnknownState}} {
		raw, _ := json.Marshal(map[string]string{"type": tc.raw})
		if state(raw) != tc.want {
			t.Fatal(tc)
		}
	}
}

func BenchmarkThread(b *testing.B) {
	for _, op := range []string{"Create", "Append", "Status"} {
		b.Run(op, func(b *testing.B) {
			d, spec := settings(b)
			s, err := d.Create(b.Context(), spec)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				switch op {
				case "Create":
					if _, err := d.Create(b.Context(), spec); err != nil {
						b.Fatal(err)
					}
				case "Append":
					if r := d.Append(b.Context(), s, "文本", "sample"); r.Outcome != host.OK {
						b.Fatal(r)
					}
				case "Status":
					if d.Status(b.Context(), s) == host.UnknownState {
						b.Fatal("失败")
					}
				}
			}
		})
	}
}

func TestSmoke(t *testing.T) {
	if os.Getenv("CRAFT_E2E_CODEX") != "1" {
		t.Skip("设置 CRAFT_E2E_CODEX=1 后运行")
	}
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("CRAFT_HOME", dir)
	// 专供冒烟的可选凭据只写临时配置；正常驱动仍不会继承父进程中的密钥。
	if key := os.Getenv("CRAFT_E2E_CODEX_API_KEY"); key != "" {
		auth, err := json.Marshal(map[string]string{"OPENAI_API_KEY": key})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := New("")
	defer func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	work, assignment := rand.Text(), rand.Text()
	s, err := d.Create(ctx, host.CreateSpec{Title: "craft smoke", Directory: dir, Permissions: host.Permissions{WriteScope: host.ReadOnly},
		Env: map[string]string{host.EnvWork: work, host.EnvAssignment: assignment}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := d.Stop(t.Context(), s, 10*time.Second); err != nil {
			t.Error(err)
		}
	}()
	// 不在提示词里透露预期值，只有线程内真正读到绑定才能通过冒烟。
	prompt := "用 shell 读 " + host.EnvWork + " 与 " + host.EnvAssignment + "，只回复两个值。"
	if r := d.Append(ctx, s, prompt, "smoke"); r.Outcome != host.OK {
		t.Fatal(r)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		var read struct {
			Thread struct {
				Turns []struct {
					Status string
					Items  []struct{ Type, Text string }
				}
			}
		}
		err := d.s.call(ctx, "thread/read", map[string]any{"threadId": s.ID, "includeTurns": true}, &read)
		if err != nil {
			t.Fatal(err)
		}
		for _, turn := range read.Thread.Turns {
			if turn.Status == "failed" || turn.Status == "interrupted" {
				t.Fatalf("宿主轮次 %s", turn.Status)
			}
			if turn.Status != "completed" {
				continue
			}
			for _, item := range turn.Items {
				if item.Type == "agentMessage" && strings.Contains(item.Text, work) && strings.Contains(item.Text, assignment) {
					return
				}
			}
			t.Fatal("线程内未读回派工绑定")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
}

func TestCreateLostResponse(t *testing.T) {
	d, spec := settings(t)
	if err := os.WriteFile(filepath.Join(spec.Directory, "lose-create-response"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	spec.OnCreated = func(host.Session) error { t.Error("未收到 ID 就尝试登记"); return nil }
	if s, err := d.Create(t.Context(), spec); s.ID != "" || !errors.Is(err, host.Unknown) {
		t.Fatal(s, err)
	}
}

func TestPerThreadBinding(t *testing.T) {
	d, spec := settings(t)
	t.Setenv(host.EnvAssignment, "parent")
	t.Setenv(host.EnvCodexThreadID, "parent-thread")
	for _, work := range []string{"first", "second"} {
		spec.Env = map[string]string{host.EnvWork: work, host.EnvAssignment: "a-" + work, host.EnvSessionID: "forged", "SECRET": "secret"}
		s, err := d.Create(t.Context(), spec)
		if err != nil || s.ID != work {
			t.Fatal(s, err)
		}
	}
}
