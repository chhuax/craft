package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/host"
)

const exampleID = "546c605a-92b1-47f2-82b4-c26e31041514"

func TestMain(m *testing.M) {
	switch {
	case len(os.Args) > 1 && os.Args[1] == "--bg":
		if err := serveCreate(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	case len(os.Args) > 1 && os.Args[1] == "--resume":
		if err := serveStream(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	case slices.Equal(os.Args[1:], []string{"agents", "--json", "--all"}):
		data, err := os.ReadFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "agents"))
		if err != nil {
			os.Exit(2)
		}
		_, _ = os.Stdout.Write(data)
		os.Exit(0)
	case slices.Equal(os.Args[1:], []string{"stop", "a49a065e"}):
		if err := os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "agents"), []byte("[]"), 0600); err != nil {
			os.Exit(2)
		}
		fmt.Println("stopped a49a065e")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fixture(t testing.TB) (*Driver, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CRAFT_HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("HOME", dir)
	return New(os.Args[0], nil), filepath.Join(dir, "agents")
}

func TestAgentStates(t *testing.T) {
	for _, tc := range []struct {
		status, state, waiting, pid string
		want                        host.State
	}{
		{"busy", "working", "", "123", host.Busy}, {"", "working", "", "null", host.Idle},
		{"idle", "done", "", "123", host.Idle}, {"waiting", "", "permission prompt", "123", host.Busy},
		{"", "stopped", "", "null", host.Gone}, {"", "", "", "123", host.UnknownState},
		{"future", "", "", "123", host.UnknownState},
	} {
		raw := fmt.Sprintf(`{"agents":[{"id":"a49a065e","sessionId":%q,"status":%q,"state":%q,"waitingFor":%q,"pid":%s}]}`, exampleID, tc.status, tc.state, tc.waiting, tc.pid)
		index, err := parseAgents(raw)
		if err != nil || index[exampleID].state() != tc.want {
			t.Fatal(tc, err, index)
		}
	}
}

func TestBatchStatusAndStop(t *testing.T) {
	d, path := fixture(t)
	now := time.Unix(0, 0)
	d.now = func() time.Time { return now }
	session := host.Session{Host: hostName, ID: exampleID}
	row := fmt.Sprintf(`[{"sessionId":%q,"id":"a49a065e","status":"busy","state":"working","pid":123}]`, exampleID)
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(row)
	if got := d.Status(t.Context(), session); got != host.Busy {
		t.Fatal(got)
	}
	write(`{"unknown":[]}`)
	if got := d.Status(t.Context(), session); got != host.Busy {
		t.Fatal(got)
	}
	now = now.Add(2 * time.Second)
	if got := d.Status(t.Context(), session); got != host.UnknownState {
		t.Fatal(got)
	}
	write(row)
	if err := d.Stop(t.Context(), session, time.Minute); err != nil {
		t.Fatal(err)
	}
	if got := d.Status(t.Context(), session); got != host.Gone {
		t.Fatal(got)
	}
}
func BenchmarkStatus(b *testing.B) {
	d, path := fixture(b)
	row := fmt.Sprintf(`[{"sessionId":%q,"status":"idle"}]`, exampleID)
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		b.Fatal(err)
	}
	s := host.Session{Host: hostName, ID: exampleID}
	if d.Status(b.Context(), s) != host.Idle {
		b.Fatal("未读到清单")
	}
	b.ReportAllocs()
	for b.Loop() {
		if d.Status(b.Context(), s) != host.Idle {
			b.Fatal("会话状态错误")
		}
	}
}

func BenchmarkStatusRefresh(b *testing.B) {
	d, path := fixture(b)
	row := fmt.Sprintf(`[{"sessionId":%q,"status":"idle"}]`, exampleID)
	if err := os.WriteFile(path, []byte(row), 0600); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		index, err := d.list(b.Context(), true)
		if err != nil || index[exampleID].state() != host.Idle {
			b.Fatal(err)
		}
	}
}
