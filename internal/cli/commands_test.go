package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

type reply struct {
	OK       bool
	Payload  map[string]json.RawMessage
	Evidence []json.RawMessage
	Error    struct {
		Code    string
		Message string
		Detail  map[string]string
	}
}

func command(t *testing.T, home string, want int, args ...string) reply {
	t.Helper()
	return boundCommand(t, home, nil, want, args...)
}

const testNow = "2026-01-01T00:00:00.000Z"

func boundCommand(t *testing.T, home string, b *app.SessionBinding, want int, args ...string) reply {
	t.Helper()
	getenv := func(k string) string {
		if k == "CRAFT_HOME" {
			return home
		}
		// 两个辅助变量都可以伪造，单靠它们不能授予角色。
		if k == "CRAFT_WORK_ID" {
			return "work"
		}
		if k == "CRAFT_WATCH_ROLE" {
			return "worker"
		}
		return ""
	}
	return checkedCommand(t, getenv, b, want, args...)
}

func checkedCommand(t *testing.T, getenv func(string) string, b *app.SessionBinding, want int, args ...string) reply {
	t.Helper()
	var out, diag bytes.Buffer
	got := runBound(append(args, "--json"), "dev", &out, &diag, getenv,
		func() (string, error) { return "", errors.New("禁止访问用户目录") }, CommandContext{Session: b}, func() string { return testNow })
	var r reply
	if got != want {
		t.Fatalf("%v 返回 %d，期望 %d，%s%s", args, got, want, &out, &diag)
	}
	raw := out.Bytes()
	if want != 0 {
		raw = diag.Bytes()
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	if r.OK != (want == 0) || want == 0 && (r.Payload == nil || r.Evidence == nil) || want != 0 && (r.Error.Code == "" || r.Error.Detail == nil) {
		t.Fatalf("信封 %s", raw)
	}
	return r
}

func TestCommandsWithoutDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	for _, args := range [][]string{{"version"}, {"--help"}} {
		command(t, home, 0, args...)
	}
	if _, err := os.Stat(filepath.Join(home, "craft.db")); !os.IsNotExist(err) {
		t.Fatalf("静态命令打开了状态库：%v", err)
	}
	r := command(t, home, 0, "watch", "request", "demo:item:1", "--text", "核对结果")
	var id string
	if err := json.Unmarshal(r.Payload["workId"], &id); err != nil || id == "" {
		t.Fatal(r.Payload)
	}
	again := command(t, home, 0, "watch", "request", `{"source":"demo","kind":"item","id":"1"}`)
	if !bytes.Equal(again.Payload["workId"], r.Payload["workId"]) {
		t.Fatal("同一对象的两个引用发起了不同工作")
	}
	for _, args := range [][]string{{"watch", "status"}, {"watch", "list", "--open", "--thread", "demo:item:1"}, {"watch", "attention"}, {"watch", "decisions", "--limit", "1"}, {"watch", "events", "--since", "0"}, {"watch", "unsubscribe", "demo:item:1"}, {"watch", "stop", id}, {"policy", "show"}} {
		command(t, home, 0, args...)
	}
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"watch", "stop", "missing"}, "unknownWork"},
		{[]string{"watch", "retry", id}, "notRetryable"},
		{[]string{"watch", "dismiss", "missing"}, "unknownProblem"},
		{[]string{"watch", "reevaluate", "missing"}, "unknownTrigger"},
	} {
		r := command(t, home, 5, tc.args...)
		if got := r.Error.Detail["code"]; got != tc.code {
			t.Errorf("错误码 %q", got)
		}
		// 给人看的只有引擎自己的结论；事务与命令层层包装的前缀放进 detail.chain，排查时才看。
		if strings.Contains(r.Error.Message, "SQLite") || r.Error.Detail["chain"] == "" || r.Error.Message == r.Error.Detail["chain"] {
			t.Errorf("拒绝消息 %q 带了内部包装，或没有保留 chain %q", r.Error.Message, r.Error.Detail["chain"])
		}
	}
}

func TestCommandArgumentsAndRoles(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{{"watch", "report", "progress", "--summary", "x"}, {"watch", "handback", "e", "--reason", "x"}, {"watch", "note", "--text", "x"}, {"watch", "follow-context", "a:b:c"}, {"watch", "mark-unrelated", "a:b:c"}, {"watch", "read", "w"}} {
		command(t, home, 4, args...)
	}
	for _, args := range [][]string{{"watch", "stop"}, {"watch", "list", "--typo"}, {"watch", "answer", "w"}, {"watch", "request", "broken"}, {"watch", "decisions", "--limit", "0"}, {"watch", "decisions", "--limit", "99999"}, {"watch", "events", "--since", "-1"}, {"policy", "set", "{}", "extra"}} {
		command(t, home, 2, args...)
	}
}

func TestHelpCommands(t *testing.T) {
	r := command(t, t.TempDir(), 0, "--help")
	for _, name := range strings.Fields("status attention list read events decisions request stop retry answer reevaluate attach unsubscribe dismiss report handback follow-context mark-unrelated") {
		if !strings.Contains(string(r.Payload["help"]), "craft watch "+name) {
			t.Errorf("帮助缺少 %s", name)
		}
	}
}

func testDB(t testing.TB, home string) *store.SQLite {
	t.Helper()
	db, err := store.OpenSQLite(t.Context(), filepath.Join(home, "craft.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return db
}

func sessionCommand(t *testing.T, home string, env map[string]string, want int, args ...string) reply {
	t.Helper()
	getenv := func(k string) string {
		if k == "CRAFT_HOME" {
			return home
		}
		return env[k]
	}
	return checkedCommand(t, getenv, app.SessionFromEnv(getenv), want, args...)
}

func TestWatchSessionBinding(t *testing.T) {
	home := t.TempDir()
	db := testDB(t, home)
	claude := engine.Session{Host: host.ClaudeCode, ID: "current"}
	other := engine.Session{Host: "codex", ID: "other"}
	ended := engine.Session{Host: claude.Host, ID: "ended"}
	work := func(id string, s engine.Session) engine.Work {
		return engine.Work{
			ID:        id,
			Execution: &engine.Execution{Session: &s, Assignment: id + "-latest"},
			Assignments: []engine.Assignment{
				{ID: id + "-old", State: engine.AssignmentEnded},
				{ID: id + "-latest", State: engine.AssignmentRunning},
			},
		}
	}
	closed := work("closed", ended)
	closed.Ended = &engine.WorkEnd{By: "human", At: testNow}
	repo := app.NewSQLiteStore(db)
	if err := repo.Atomic(t.Context(), func(u engine.Unit) error {
		for _, w := range []engine.Work{work("mine", claude), work("theirs", other), closed} {
			if err := u.PutWork(w); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	otherEnv := func() map[string]string {
		return host.BindingEnv(host.Session(other), map[string]string{host.EnvWork: "theirs", host.EnvAssignment: "theirs-latest"})
	}
	for _, name := range []string{"native", "stale"} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{}
			if name == "stale" {
				env = otherEnv()
			}
			env[host.EnvClaudeSessionID] = claude.ID
			report := sessionCommand(t, home, env, 0, "watch", "report", "progress", "--summary", name)
			if got := string(report.Payload["workId"]); got != `"mine"` {
				t.Fatalf("report 写入 %s，期望 mine", got)
			}
			facts := sessionCommand(t, home, env, 0, "watch", "read", "mine")
			var saved engine.Report
			if err := json.Unmarshal(facts.Payload["report"], &saved); err != nil || saved.Summary != name || string(facts.Payload["assignmentId"]) != `"mine-latest"` {
				t.Fatalf("最新派工未收到回报: %v %v", saved, err)
			}
			if facts := sessionCommand(t, home, otherEnv(), 0, "watch", "read", "theirs"); facts.Payload["report"] != nil {
				t.Fatal("另一个 Work 收到了回报")
			}
			sessionCommand(t, home, env, 4, "watch", "read", "theirs")
			sessionCommand(t, home, env, 4, "watch", "list")
			sessionCommand(t, home, env, 0, "watch", "turn-ended")
		})
	}
	for _, id := range []string{"unregistered", ended.ID} {
		t.Run(id, func(t *testing.T) {
			env := otherEnv()
			env[host.EnvClaudeSessionID] = id
			sessionCommand(t, home, env, 0, "watch", "list")
			sessionCommand(t, home, env, 4, "watch", "report", "progress", "--summary", "x")
		})
	}
	t.Run("person", func(t *testing.T) {
		sessionCommand(t, home, nil, 0, "watch", "list")
		sessionCommand(t, home, nil, 4, "watch", "read", "mine")
	})
	t.Run("codex", func(t *testing.T) {
		env := otherEnv()
		env[host.EnvCodexThreadID] = other.ID
		sessionCommand(t, home, env, 0, "watch", "read", "theirs")
		env[host.EnvClaudeSessionID] = claude.ID
		delete(env, host.EnvSessionID)
		sessionCommand(t, home, env, 0, "watch", "read", "theirs")
		sessionCommand(t, home, env, 4, "watch", "read", "mine")
	})
}

func TestSessionLookupStorage(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{host.EnvClaudeSessionID: "session", host.EnvWork: "stale"}
	path := filepath.Join(home, "craft.db")
	t.Run("missing", func(t *testing.T) {
		sessionCommand(t, home, env, 0, "watch", "list")
		sessionCommand(t, home, env, 4, "watch", "turn-ended")
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("角色查询创建了状态库: %v", err)
		}
	})
	if err := os.WriteFile(path, []byte("无效的状态库"), 0600); err != nil {
		t.Fatal(err)
	}
	sessionCommand(t, home, env, 0, "version")
	sessionCommand(t, home, nil, 4, "watch", "turn-ended")
	codex := host.BindingEnv(host.Session{Host: "codex"}, map[string]string{host.EnvWork: "work", host.EnvAssignment: "latest"})
	codex[host.EnvCodexThreadID] = "thread"
	sessionCommand(t, home, codex, 4, "watch", "list")
	sessionCommand(t, home, env, 1, "watch", "turn-ended")
}

func TestWorkerAuthorityAndHumanCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	b := app.SessionBinding{Session: engine.Session{Host: "local", ID: "session"}, Assignment: "a"}
	r := command(t, home, 0, "watch", "request", "src:item:start", "--text", "initial")
	var id string
	if err := json.Unmarshal(r.Payload["workId"], &id); err != nil {
		t.Fatal(err)
	}
	db := testDB(t, home)
	repo := app.NewSQLiteStore(db)
	var origin string
	err := repo.Atomic(t.Context(), func(u engine.Unit) error {
		w, err := u.GetWork(id)
		if err != nil {
			return err
		}
		w.Assignments = []engine.Assignment{{ID: "a", State: engine.AssignmentRunning}}
		w.Execution = &engine.Execution{Session: &b.Session, Assignment: "a"}
		origin = w.Origin.TriggerID
		if err := u.PutWork(*w); err != nil {
			return err
		}
		ref := engine.NewThreadRef("src", "item", "other")
		key := engine.ThreadKey(ref)
		if err := u.PutTrigger(engine.Trigger{ID: "new", Source: "src", Event: "signal", Threads: []engine.ThreadRef{ref}, Text: "private fact"}); err != nil {
			return err
		}
		if err := u.PutThread(engine.Thread{Key: key, Ref: ref, Evaluating: &engine.Evaluation{ID: "ev", TriggerIDs: []string{"new"}}}); err != nil {
			return err
		}
		return u.PutDecision(engine.Decision{ID: "problem", Kind: engine.Unavailable, ThreadKey: key, TriggerIDs: []string{"problematic"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	call := func(want int, args ...string) reply {
		return boundCommand(t, home, &b, want, append([]string{"watch"}, args...)...)
	}
	facts := call(0, "read", id)
	if bytes.Contains(facts.Payload["triggers"], []byte("private fact")) || !bytes.Contains(facts.Payload["triggers"], []byte(origin)) {
		t.Fatal("读取越过工作输入边界或缺少起源")
	}
	if !bytes.Contains(facts.Payload["execution"], []byte("local")) || !bytes.Contains(facts.Payload["mandate"], []byte("personRequest")) {
		t.Fatal("执行方重读缺少授权依据或工作位置")
	}
	call(4, "read", "somebody-else")
	call(4, "request", "src:item:other")
	call(2, "report", "waiting", "--summary", "x")
	call(2, "handback", origin, "--reason", strings.Repeat("长", 301))
	call(0, "report", "progress", "--summary", "处理中", "--question", "需要继续吗")
	command(t, home, 0, "watch", "answer", id, "--text", "继续")
	call(0, "follow-context", "src:item:context")
	call(0, "mark-unrelated", "src:item:context")
	command(t, home, 0, "watch", "attach", id, "new")
	call(0, "handback", "new", "--reason", "独立处理")
	command(t, home, 0, "watch", "reevaluate", "new")
	command(t, home, 0, "watch", "dismiss", "problem")
	call(6, "note", "--text", "备注")
	call(0, "report", "failed", "--summary", "重试")
	// 显式回答与失败报告同毫秒的先后不能靠实时时钟判断。
	err = repo.Atomic(t.Context(), func(u engine.Unit) error {
		w, err := u.GetWork(id)
		if err != nil {
			return err
		}
		w.Delivery = &engine.DeliveryFailure{Error: "offline", At: testNow}
		return u.PutWork(*w)
	})
	if err != nil {
		t.Fatal(err)
	}
	command(t, home, 0, "watch", "retry", id)
	stale := b
	stale.Assignment = "old"
	if got := boundCommand(t, home, &stale, 5, "watch", "follow-context", "src:item:bad"); got.Error.Detail["code"] != "assignmentEnded" {
		t.Fatal(got.Error)
	}
	stale = b
	stale.Session.Host = "other"
	if got := boundCommand(t, home, &stale, 5, "watch", "read", id); got.Error.Detail["code"] != "noWorkForSession" {
		t.Fatal(got.Error)
	}
	command(t, home, 0, "watch", "stop", id)
	if got := call(5, "read", id); got.Error.Detail["code"] != "workEnded" {
		t.Fatal(got.Error)
	}
}

func TestExitCodes(t *testing.T) {
	for _, code := range strings.Fields("workEnded workStopped unknownTrigger unknownWork unknownAssignment assignmentEnded evalFinished workerDecides notAdmissible notExplicit notReached handedBack handedBackHere mandateRevoked notRetryable unknownProblem noAttention threadAlreadyOwned noWorkForSession sourceThread followedThread notFollowing questionWithResult askerWithoutQuestion notAsker notAnswerable ackEnded ackNoChannel ackNotPerson notReady notAttention notQuestion noAnswer noChannel") {
		t.Run(code, func(t *testing.T) {
			var b bytes.Buffer
			n := failure(&b, true, fmt.Errorf("命令: %w", &engine.UserError{Code: code}))
			var envelope reply
			if err := json.Unmarshal(b.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			if n != 5 || envelope.Error.Detail["code"] != code {
				t.Fatal(n, envelope.Error)
			}
		})
	}
}

func TestWatchPagesAndEvidence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	db := testDB(t, home)
	ref := engine.NewThreadRef("feed", "item", "1")
	thread := engine.ThreadKey(ref)
	long := strings.Repeat("事实", 1800)
	err := app.NewSQLiteStore(db).Atomic(t.Context(), func(u engine.Unit) error {
		if err := u.PutThread(engine.Thread{Key: thread, Ref: ref}); err != nil {
			return err
		}
		for n := range 123 {
			id := fmt.Sprintf("%03d", n)
			w := engine.Work{ID: id, Title: long, Attention: &engine.Question{ID: "q" + id, Question: long}}
			if n%2 == 0 {
				w.Ended = &engine.WorkEnd{By: "human"}
			}
			if err := u.PutWork(w); err != nil {
				return err
			}
			d := engine.Decision{ID: id, ThreadKey: thread, Kind: engine.Ignore, TriggerIDs: []string{id}, Audit: map[string]json.RawMessage{"would_accept": []byte("true"), "evidence": []byte(`"record"`)}}
			if n%2 == 0 {
				d.ThreadKey = "second:item:1"
			}
			if n == 0 || n == 1 {
				d.Kind = engine.Unavailable
				d.ThreadKey = thread
				d.TriggerIDs = []string{"superseded"}
			}
			if n == 2 {
				d.TriggerIDs = []string{"superseded"}
				d.ThreadKey = thread
			}
			if err := u.PutDecision(d); err != nil {
				return err
			}
			raw, err := json.Marshal(long)
			if err != nil {
				return err
			}
			if err := u.Emit(engine.Event{Type: "work.changed", At: testNow, Data: map[string]json.RawMessage{"text": raw}}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := command(t, home, 0, "watch", "status")
	if string(s.Payload["threads"]) != "1" || string(s.Payload["works"]) != "123" || string(s.Payload["attention"]) != "61" {
		t.Fatal(s.Payload)
	}
	for _, tc := range []struct {
		cmd          []string
		cursor, next string
		total, size  int
	}{
		{[]string{"list", "--open"}, "--cursor", "nextCursor", 61, 50},
		{[]string{"attention"}, "--cursor", "nextCursor", 61, 50},
		{[]string{"events"}, "--since", "nextSince", 123, 50},
		{[]string{"decisions", "--kind", "ignore", "--source", "feed", "--would-accept", "--limit", "17"}, "--cursor", "nextCursor", 61, 17},
	} {
		t.Run(tc.cmd[0], func(t *testing.T) {
			var cursor string
			count := 0
			seen := map[string]bool{}
			for {
				args := append([]string{"watch"}, tc.cmd...)
				if cursor != "" {
					args = append(args, tc.cursor, cursor)
				}
				r := command(t, home, 0, args...)
				var items []map[string]json.RawMessage
				if err := json.Unmarshal(r.Payload["items"], &items); err != nil {
					t.Fatal(err)
				}
				if len(items) > tc.size || bytes.Contains(r.Payload["items"], []byte(long)) {
					t.Fatal("输出未设上限")
				}
				if tc.cmd[0] == "decisions" && (len(r.Evidence) != len(items) || !bytes.Contains(r.Evidence[0], []byte("record"))) {
					t.Fatal("缺少决策依据")
				}
				for _, item := range items {
					k := string(item["id"])
					if k == "" {
						k = string(item["rev"])
					}
					if k == "" || seen[k] {
						t.Fatal("重复或缺少标识", k)
					}
					seen[k] = true
				}
				count += len(items)
				if next := r.Payload[tc.next]; next != nil {
					cursor = strings.Trim(string(next), `"`)
				} else {
					break
				}
			}
			if count != tc.total {
				t.Errorf("分页命中 %d，期望 %d", count, tc.total)
			}
		})
	}
}

func TestPolicyCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	initial := command(t, home, 0, "policy", "show")
	request := `{"paused":true,"allowedSenders":[{"source":"feed","id":"person"}],"rules":[{"id":"r","enabled":true,"instruction":"确认"}]}`
	set := command(t, home, 0, "policy", "set", request)
	if bytes.Equal(set.Payload["version"], initial.Payload["version"]) || !bytes.Contains(set.Payload["settings"], []byte("确认")) {
		t.Fatal(set)
	}
	r := command(t, home, 0, "policy", "set", `{"paused":false}`)
	if !bytes.Equal(r.Payload["version"], set.Payload["version"]) || !bytes.Contains(r.Payload["settings"], []byte(`"paused":false`)) {
		t.Fatal(r)
	}
	for _, bad := range []string{`null`, `{"paused":null}`, `{"paused":1}`, `{"rules":[{}]}`, `{"rules":[],"rules":[]}`, `{"allowedSenders":[{"id":"x"}]}`, `{"events":{"craft:person.request":{"mode":"off"}}}`, `{"unknown":2}`} {
		command(t, home, 2, "policy", "set", bad)
	}
	after := command(t, home, 0, "policy", "show")
	if !bytes.Equal(after.Payload["settings"], r.Payload["settings"]) {
		t.Fatal("失败的修改改变了设置")
	}
}

func TestPolicySetRollsBackWithWork(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	db := testDB(t, home)
	ctx := t.Context()
	const key = "feed:change"
	// 存入运行层实际使用的扩展目录，覆盖项必须使用上一次生效的默认模式和规则。
	catalog := []engine.EventDef{{Source: "feed", Event: "change", Default: engine.ModeAuto, Rule: &engine.EventRule{Name: "默认规则", Instruction: "核对"}}}
	r, err := app.NewRuntime(db, alarm.RealClock{}, app.ObjectKinds{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyPolicy(ctx, engine.Settings{}, catalog); err != nil {
		t.Fatal(err)
	}
	repo := app.NewSQLiteStore(db)
	err = repo.Atomic(ctx, func(u engine.Unit) error {
		return u.PutWork(engine.Work{ID: "affected", Mandate: engine.Mandate{Strength: engine.Inferred, Basis: engine.MandateBasis{Source: "feed", Event: "change", Rules: []string{"feed__change"}}}})
	})
	if err != nil {
		t.Fatal(err)
	}
	// 事件目录不含对象的 closable 声明，不能把未知对象当作不可关闭来绕开互斥。
	command(t, home, 6, "watch", "request", "feed:item:new")
	prior := command(t, home, 0, "policy", "show")
	if !bytes.Contains(prior.Payload["settings"], []byte(key)) {
		t.Fatal("未显示默认事件模式")
	}
	err = db.Write(ctx, func(tx *store.SQLiteTx) error { return tx.Put("watch_kv", "effect.seq", []byte(`"broken"`)) })
	if err != nil {
		t.Fatal(err)
	}
	const update = `{"events":{"feed:change":{"mode":"off"}}}`
	command(t, home, 1, "policy", "set", update)
	after := command(t, home, 0, "policy", "show")
	if !bytes.Equal(after.Payload["version"], prior.Payload["version"]) {
		t.Fatal("部分提交新版本")
	}
	check := func(want int) {
		t.Helper()
		err := repo.Read(ctx, func(r engine.Reader) error {
			w, err := r.GetWork("affected")
			if err != nil {
				return err
			}
			has, err := r.HasEffect("delivery", "affected")
			if err != nil {
				return err
			}
			if len(w.Inputs) != want || has != (want > 0) {
				return fmt.Errorf("工作输入 %d，投递 effect %v", len(w.Inputs), has)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	check(0)
	if err := db.Write(ctx, func(tx *store.SQLiteTx) error { return tx.Delete("watch_kv", "effect.seq") }); err != nil {
		t.Fatal(err)
	}
	command(t, home, 0, "policy", "set", update)
	check(1)
}

func TestReadWhileWriterActive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	db := testDB(t, home)
	err := db.Write(t.Context(), func(tx *store.SQLiteTx) error {
		if err := tx.Put("watch_thread", "one", []byte(`{"key":"one"}`)); err != nil {
			return err
		}
		got := command(t, home, 0, "watch", "status")
		if string(got.Payload["threads"]) != "0" {
			return errors.New("只读连接看到了未提交数据")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := command(t, home, 0, "watch", "status"); string(got.Payload["threads"]) != "1" {
		t.Fatal("提交后总数没有变化")
	}
}

func TestRequestUsesDeclaredObjectKind(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"demo","protocol":1,"version":"1","exec":["./unused"],"watch_sources":[{"id":"feed","thread_kinds":[{"id":"item","closable":true}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, diag bytes.Buffer
	code := runBound([]string{"watch", "request", "feed:item:one", "--json"}, "dev", &out, &diag, os.Getenv, os.UserHomeDir, CommandContext{ExtensionDirs: []string{root}}, func() string { return testNow })
	if code != 0 {
		t.Fatal(code, diag.String())
	}
	var r reply
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := json.Unmarshal(r.Payload["workId"], &id); err != nil {
		t.Fatal(err)
	}
	err := app.NewSQLiteStore(testDB(t, home)).Read(t.Context(), func(r engine.Reader) error {
		w, err := r.GetWork(id)
		if err != nil {
			return err
		}
		if len(w.Follows) != 1 || !w.Follows[0].Own {
			return errors.New("已声明可关闭的对象没有登记归属")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWorkReadsOnlyReachedPages(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	db := testDB(t, home)
	b := app.SessionBinding{Session: engine.Session{Host: "local", ID: "s"}, Assignment: "a"}
	repo := app.NewSQLiteStore(db)
	long := strings.Repeat("内容", 2000)
	err := repo.Atomic(t.Context(), func(u engine.Unit) error {
		w := engine.Work{ID: "work", Execution: &engine.Execution{Session: &b.Session}, Assignments: []engine.Assignment{{ID: "a", State: engine.AssignmentRunning}}}
		ref := engine.NewThreadRef("src", "item", "x")
		for i := range 55 {
			id := fmt.Sprintf("%02d", i)
			if err := u.PutTrigger(engine.Trigger{ID: id, Threads: []engine.ThreadRef{ref}, Text: long}); err != nil {
				return err
			}
			if err := u.LinkTrigger(id, engine.ThreadKey(ref)); err != nil {
				return err
			}
			if i < 54 {
				w.Inputs = append(w.Inputs, engine.WorkInput{Kind: "trigger", ID: "in-" + id, TriggerID: id})
			}
		}
		return u.PutWork(w)
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, page := range []struct {
		offset string
		length int
		more   bool
	}{{"0", 50, true}, {"50", 4, false}, {"999", 0, false}} {
		r := boundCommand(t, home, &b, 0, "watch", "read", "work", "--offset", page.offset)
		var triggers []engine.Trigger
		if err := json.Unmarshal(r.Payload["triggers"], &triggers); err != nil {
			t.Fatal(err)
		}
		if len(triggers) != page.length || (r.Payload["nextOffset"] != nil) != page.more {
			t.Fatal("资源页边界错误")
		}
		for _, tr := range triggers {
			if tr.ID == "54" || tr.Text == long {
				t.Fatal("输出越过事件归属或长度上限")
			}
		}
	}
}

func TestOutboundNeedsDaemon(t *testing.T) {
	home := t.TempDir()
	for _, args := range [][]string{{"act", "write", "--params", "{}", "--yes"}, {"outbox", "list"}, {"outbox", "show", "k"}, {"outbox", "confirm", "k", "--yes"}, {"outbox", "withdraw", "k"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) { command(t, home, 6, args...) })
	}
}

// 真实入口（cmd/craft/main.go）不会注入扩展目录：`watch request` 要核对对象类型是否可关闭时，
// 必须自己从 <CRAFT_HOME>/extensions 装载声明。此前只有 daemon 与 ext 两组命令会自动装配，
// 测试又总是手工注入，所以联调时（事件目录非空的库里）`watch request` 在真实二进制里直接失败。
func TestRequestFindsExtensionsInStateDirWithoutInjection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	dir := filepath.Join(home, "extensions", "demo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"id":"demo","protocol":1,"version":"1","exec":["./unused"],"watch_sources":[{"id":"feed","thread_kinds":[{"id":"item","closable":true}],"events":[{"id":"change","default":"observe","strength":"explicit"}]}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	// 事件目录非空才会触发「必须有对象声明」的核对。
	db := testDB(t, home)
	r, err := app.NewRuntime(db, alarm.RealClock{}, app.ObjectKinds{}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.ApplyPolicy(t.Context(), engine.Settings{}, []engine.EventDef{{Source: "feed", Event: "change", Default: engine.ModeObserve}}); err != nil {
		t.Fatal(err)
	}
	command(t, home, 0, "watch", "request", "feed:item:real")
}
