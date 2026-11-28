package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/host"
)

func serveStream() error {
	if !slices.Equal(os.Args[1:], []string{"--resume", exampleID, "--input-format", "stream-json", "--output-format", "stream-json", "--replay-user-messages", "--verbose", "-p"}) {
		return errors.New("恢复参数无效")
	}
	if os.Getenv(host.EnvSessionID) != exampleID || os.Getenv(host.EnvAssignment) != "a" || os.Getenv("TEST_SECRET") != "" {
		return errors.New("绑定错误")
	}
	input := bufio.NewScanner(os.Stdin)
	send := json.NewEncoder(os.Stdout)
	for input.Scan() {
		var u userFrame
		if err := json.Unmarshal(input.Bytes(), &u); err != nil {
			return err
		}
		if !strings.HasPrefix(u.Message.Content, "[craft:delivery:") {
			return errors.New("缺少标记")
		}
		if strings.HasSuffix(u.Message.Content, "残缺回执") {
			return send.Encode(map[string]string{"type": "result", "session_id": exampleID})
		}
		if strings.HasSuffix(u.Message.Content, "丢失回执") {
			return nil
		}
		if err := send.Encode(u); err != nil {
			return err
		}
		if strings.HasSuffix(u.Message.Content, "结束") {
			return send.Encode(map[string]any{"type": "result", "is_error": false, "session_id": exampleID, "result": "共同答复"})
		}
	}
	return input.Err()
}

func restored(t testing.TB) (*Driver, host.Session, string) {
	t.Helper()
	d, path := fixture(t)
	t.Setenv("TEST_SECRET", "parent")
	s := host.Session{Host: hostName, ID: exampleID}
	spec := host.CreateSpec{Directory: filepath.Dir(path), Env: map[string]string{host.EnvAssignment: "a"}}
	d.lookup = func(context.Context, host.Session) (host.CreateSpec, error) { return spec, nil }
	row := fmt.Sprintf(`[{"sessionId":%q,"id":"a49a065e","status":"idle"}]`, exampleID)
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d, s, path
}

func TestResumeAndMerge(t *testing.T) {
	d, s, _ := restored(t)
	for _, pair := range [][2]string{{"a", "首条"}, {"b", "结束"}} {
		if r := d.Append(t.Context(), s, pair[1], pair[0]); r.Outcome != host.OK {
			t.Fatal(r)
		}
	}
	d.workers.Wait()
	if reply, err := d.LastReply(t.Context(), s); err != nil || reply != "共同答复" {
		t.Fatal(reply, err)
	}
	if r := d.Append(t.Context(), s, "丢失回执", "lost"); r.Outcome != host.Unknown {
		t.Fatal(r)
	}
}

func TestIncompleteResult(t *testing.T) {
	d, s, _ := restored(t)
	if r := d.Append(t.Context(), s, "残缺回执", "incomplete"); r.Outcome != host.Unknown {
		t.Fatal(r)
	}
}

func TestBusyUnowned(t *testing.T) {
	d, s, path := restored(t)
	row := fmt.Sprintf(`[{"sessionId":%q,"status":"waiting","waitingFor":"permission prompt"}]`, s.ID)
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		t.Fatal(err)
	}
	if r := d.Append(t.Context(), s, "文本", "valid"); r.Outcome != host.Later {
		t.Fatal(r)
	}
}

func TestPermissions(t *testing.T) {
	for _, p := range []host.Permissions{{NoNetwork: true}, {WriteScope: host.Worktree}, {WriteScope: host.ReadOnly, Tools: []string{"Bash"}}, {Tools: []string{"--settings"}}} {
		if _, err := permissionArgs(p, ""); !errors.Is(err, host.Unsupported) {
			t.Fatal(p, err)
		}
	}
	for _, tools := range [][]string{{"Read"}, {}, {"Bash", "Read"}} {
		args, err := permissionArgs(host.Permissions{Tools: tools}, "")
		if err != nil || !slices.Contains(args, "--tools") || !slices.Contains(args, strings.Join(tools, ",")) {
			t.Fatal(args, err)
		}
	}
}

func BenchmarkAppend(b *testing.B) {
	d, s, _ := restored(b)
	for b.Loop() {
		if r := d.Append(b.Context(), s, "文本", "bench"); r.Outcome != host.OK {
			b.Fatal(r)
		}
	}
}

func TestSmoke(t *testing.T) {
	if os.Getenv("CRAFT_E2E_CLAUDE") != "1" {
		t.Skip("设置 CRAFT_E2E_CLAUDE=1 后运行")
	}
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("APPDATA", dir)
	t.Setenv("LOCALAPPDATA", dir)
	t.Setenv("CRAFT_HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	// 仅使用明确提供给本次冒烟的独立设置，不读取用户的登录配置。
	if settings := os.Getenv("CRAFT_E2E_CLAUDE_SETTINGS_JSON"); settings != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(settings), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := New("", nil)
	defer func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err := d.list(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	t.Run("worker", func(t *testing.T) {
		s, err := d.Create(t.Context(), host.CreateSpec{Directory: dir, Title: "craft smoke", Permissions: host.Permissions{WriteScope: host.ReadOnly}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.Stop(t.Context(), s, 10*time.Second); err != nil {
				t.Error(err)
			}
		}()
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for d.Status(ctx, s) != host.Idle {
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-tick.C:
			}
		}
		if r := d.Append(ctx, s, "只回复 OK", "smoke"); r.Outcome != host.OK {
			t.Fatal(r)
		}
		for {
			if reply, err := d.LastReply(ctx, s); err == nil {
				if !strings.Contains(strings.ToUpper(reply), "OK") {
					t.Fatal("未收到预期的回复")
				}
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-tick.C:
			}
		}
	})
}

func TestStopWhileResolving(t *testing.T) {
	d, s, _ := restored(t)
	entered := make(chan struct{})
	d.lookup = func(ctx context.Context, _ host.Session) (host.CreateSpec, error) {
		close(entered)
		<-ctx.Done()
		return host.CreateSpec{}, ctx.Err()
	}
	done := make(chan host.Result, 1)
	go func() { done <- d.Append(t.Context(), s, "文本", "race") }()
	<-entered
	if err := d.Stop(t.Context(), s, time.Minute); err != nil {
		t.Fatal(err)
	}
	if r := <-done; r.Outcome != host.Later {
		t.Fatal(r)
	}
}
