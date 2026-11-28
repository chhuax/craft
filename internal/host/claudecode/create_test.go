package claudecode

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/host"
)

func serveCreate() error {
	if len(os.Args) < 6 || os.Args[2] != "--name" || !slices.Equal(os.Args[4:6], []string{"--output-format", "json"}) {
		return errors.New("后台创建参数无效")
	}
	// 带首条时正文必须在 "--" 之后，且带投递标记。
	if len(os.Args) != 6 && (len(os.Args) != 8 || os.Args[6] != "--" || !strings.HasPrefix(os.Args[7], "[craft:delivery:")) {
		return errors.New("首条参数无效")
	}
	if os.Getenv(host.EnvRole) != "worker" || os.Getenv(host.EnvHost) != hostName || os.Getenv(host.EnvSessionID) != "" || os.Getenv(host.EnvAssignment) != "a" || os.Getenv(host.EnvWork) != "w" || os.Getenv("PRIVATE_TOKEN") != "" {
		return errors.New("新会话绑定错误")
	}
	switch os.Args[3] {
	case "environment":
		for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_MODEL", "HTTPS_PROXY", "http_proxy", "SSL_CERT_FILE", "SITE_AUTH"} {
			if os.Getenv(name) != "allowed" {
				return fmt.Errorf("缺少 %s", name)
			}
		}
		for _, name := range []string{"OPENAI_API_KEY", "CODEX_HOME", "DATABASE_PASSWORD", host.EnvCodexThreadID} {
			if _, ok := os.LookupEnv(name); ok {
				return fmt.Errorf("误传 %s", name)
			}
		}
	case "fail":
		return errors.New("创建失败")
	case "unknown":
		fmt.Println("backgrounded")
		return nil
	}
	row := fmt.Sprintf(`[{"id":"a49a065e","sessionId":%q,"status":"idle","state":"done"}]`, exampleID)
	if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "agents"), []byte(row), 0600); err != nil {
		return err
	}
	fmt.Printf("backgrounded · a49a065e · %s\n", os.Args[3])
	return nil
}

func creation(t testing.TB) (*Driver, host.CreateSpec) {
	t.Helper()
	d, path := fixture(t)
	t.Setenv("PRIVATE_TOKEN", "parent-secret")
	t.Setenv(host.EnvSessionID, "parent")
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Error(err)
		}
	})
	return d, host.CreateSpec{Directory: filepath.Dir(path), Title: "首轮", Env: map[string]string{host.EnvAssignment: "a", host.EnvWork: "w", host.EnvSessionID: "forged", "PRIVATE_TOKEN": "forged"}}
}

func TestCreateAndBootstrap(t *testing.T) {
	d, spec := creation(t)
	var registered host.Session
	spec.OnCreated = func(s host.Session) error { registered = s; return nil }
	spec.Prompt, spec.DeliveryID = "结束", "first"
	s, err := d.Create(t.Context(), spec)
	if err != nil || s != registered || s.ID != exampleID {
		t.Fatal(s, registered, err)
	}
}

func TestCreateFailure(t *testing.T) {
	d, spec := creation(t)
	for _, name := range []string{"fail", "unknown"} {
		spec.Title = name
		if s, err := d.Create(t.Context(), spec); !errors.Is(err, host.Unknown) || s.ID != "" {
			t.Fatal(s, err)
		}
	}
	spec.Title = "登记失败"
	spec.OnCreated = func(host.Session) error { return context.Canceled }
	spec.DeliveryID, spec.Prompt = "one", "结束"
	if s, err := d.Create(t.Context(), spec); s.ID != exampleID || !errors.Is(err, host.Unknown) {
		t.Fatal(s, err)
	}
	if _, err := d.LastReply(t.Context(), host.Session{Host: hostName, ID: exampleID}); !errors.Is(err, host.Unsupported) {
		t.Fatal(err)
	}
	spec.DeliveryID = "bad/id"
	if _, err := d.Create(t.Context(), spec); !errors.Is(err, host.Unsupported) {
		t.Fatal(err)
	}
}

func BenchmarkCreate(b *testing.B) {
	d, spec := creation(b)
	b.ReportAllocs()
	for b.Loop() {
		s, err := d.Create(b.Context(), spec)
		if err != nil || s.ID != exampleID {
			b.Fatal(s, err)
		}
	}
}

func TestEnvironment(t *testing.T) {
	d, spec := creation(t)
	for _, name := range []string{"ANTHROPIC_API_KEY", "CLAUDE_MODEL", "HTTPS_PROXY", "http_proxy", "SSL_CERT_FILE", "SITE_AUTH", "OPENAI_API_KEY", "CODEX_HOME", "DATABASE_PASSWORD", host.EnvCodexThreadID, host.EnvHost, host.EnvRole, host.EnvWork, host.EnvAssignment} {
		t.Setenv(name, "allowed")
	}
	if err := os.WriteFile(filepath.Join(spec.Directory, "host.json"), []byte(`{"envPassthrough":["SITE_AUTH","CRAFT_SESSION_ID","CRAFT_WORK_ID","CRAFT_ASSIGNMENT_ID","CRAFT_WATCH_ROLE","CRAFT_SESSION_HOST","CODEX_THREAD_ID"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	spec.Title = "environment"
	if _, err := d.Create(t.Context(), spec); err != nil {
		t.Fatal(err)
	}
}
