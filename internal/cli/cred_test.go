package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/chhuax/craft/internal/app"
	"github.com/zalando/go-keyring"
)

func TestCredentialCommands(t *testing.T) {
	keyring.MockInit()
	t.Setenv("CRAFT_HOME", t.TempDir())
	input, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
	}()
	const secret = "  stdin-value  "
	if _, err := input.WriteString(secret + "\r\nignored\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = original })
	for _, args := range [][]string{{"cred", "login", "demo"}, {"cred", "list"}, {"cred", "logout", "demo"}} {
		var out, diag bytes.Buffer
		if code := Run(append(args, "--json"), "dev", &out, &diag); code != 0 || strings.Contains(out.String()+diag.String(), strings.TrimSpace(secret)) {
			t.Fatal("凭据命令失败或泄露输入", code)
		}
		if args[1] == "login" {
			got, err := keyring.Get("craft", "demo/default")
			if err != nil || got != secret {
				t.Fatal("没有保留 stdin 第一行", err)
			}
		}
		if args[1] == "list" && (!strings.Contains(out.String(), "keychain") || !strings.Contains(out.String(), "demo")) {
			t.Fatal("缺少后端或条目信息")
		}
	}
	for _, arg := range []string{"argv-secret", "--argv-secret"} {
		var out, diag bytes.Buffer
		if code := Run([]string{"cred", "login", "demo", arg, "--json"}, "dev", &out, &diag); code != 2 || strings.Contains(diag.String(), arg) {
			t.Fatal("额外参数未被安全拒绝")
		}
	}
	for _, args := range [][]string{{"cred", "list"}, {"cred", "logout", "demo", "--name", "work"}, {"cred", "login", "demo"}} {
		boundCommand(t, t.TempDir(), &app.SessionBinding{}, 4, args...)
	}
}

func TestNamedCredentialFile(t *testing.T) {
	keyring.MockInitWithError(errors.New("offline"))
	home := t.TempDir()
	input, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := input.Close(); err != nil {
			t.Error(err)
		}
	})
	original := os.Stdin
	os.Stdin = input
	t.Cleanup(func() { os.Stdin = original })
	const secret = "pipe-without-newline"
	_, err = w.WriteString(secret)
	if err := errors.Join(err, w.Close()); err != nil {
		t.Fatal(err)
	}
	command(t, home, 0, "cred", "login", "git-hub", "--name", "site.2")
	r := command(t, home, 0, "cred", "list")
	if string(r.Payload["backend"]) != `"file"` || len(r.Payload["warning"]) == 0 || !bytes.Contains(r.Payload["entries"], []byte("site.2")) || bytes.Contains(r.Payload["entries"], []byte(secret)) {
		t.Fatal("文件后端列表缺少登记信息或保护范围")
	}
	command(t, home, 0, "cred", "logout", "git-hub", "--name", "site.2")
	if r := command(t, home, 0, "cred", "list"); string(r.Payload["entries"]) != "[]" {
		t.Fatal("仍有登记")
	}
	for _, args := range [][]string{{"cred", "login", "invalid/provider"}, {"cred", "logout", "ok", "--name", "invalid name"}} {
		command(t, home, 2, args...)
	}
}
