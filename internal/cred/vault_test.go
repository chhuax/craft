package cred

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type fakeRing struct {
	values map[string]string
	probes int
	down   bool
	fail   error
}

func (r *fakeRing) get(key string) (string, error) {
	if key == probeKey {
		r.probes++
		if r.down {
			return "", errors.New("offline")
		}
	}
	v, ok := r.values[key]
	if !ok {
		return "", errMissing
	}
	return v, nil
}
func (r *fakeRing) set(key, value string) error {
	if r.fail != nil {
		return r.fail
	}
	r.values[key] = value
	return nil
}
func (r *fakeRing) remove(key string) error { delete(r.values, key); return nil }

func TestVaultRoundTrip(t *testing.T) {
	const secret = "  private-example  "
	for _, mode := range []string{"keychain", "file"} {
		t.Run(mode, func(t *testing.T) {
			home := t.TempDir()
			ring := &fakeRing{down: mode == "file", values: map[string]string{}}
			v := newVault(home, ring)
			if err := v.Set("demo", "work_1.2", secret); err != nil {
				t.Fatal(err)
			}
			got, ok, err := v.Get("demo", "work_1.2")
			if err != nil || !ok || got != secret {
				t.Fatal("凭据往返失败", err)
			}
			name, entries, err := v.List()
			if err != nil || name != mode || len(entries) != 1 || entries[0] != (Entry{"demo", "work_1.2", true}) {
				t.Fatal(name, entries, err)
			}
			raw, err := json.Marshal(entries)
			if err != nil || bytes.Contains(raw, []byte(secret)) {
				t.Fatal("列表泄露凭据", err)
			}
			if mode == "keychain" {
				delete(ring.values, "demo/work_1.2")
				_, entries, err = v.List()
				if err != nil || len(entries) != 1 || entries[0].Exists {
					t.Fatal("钥匙串被外部删除", err)
				}
				ring.fail = errors.New(secret)
				err = v.Set("demo", "work_1.2", secret)
				if err == nil || strings.Contains(err.Error(), secret) {
					t.Fatal("后端错误泄露凭据")
				}
			}
			if err := v.Delete("demo", "work_1.2"); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := v.Get("demo", "work_1.2"); err != nil || ok {
				t.Fatal("删除后仍可读取", err)
			}
			_, entries, err = v.List()
			if err != nil || len(entries) != 0 || ring.probes != 1 {
				t.Fatal(entries, err, ring.probes)
			}
		})
	}
}

func TestFileContents(t *testing.T) {
	home := t.TempDir()
	v := newVault(home, &fakeRing{down: true})
	const secret = "disk-plaintext-sentinel"
	save := func() []byte {
		t.Helper()
		if err := v.Set("demo", "default", secret); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(home, "credentials.enc"))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	first := save()
	second := save()
	if bytes.Equal(first, second) {
		t.Fatal("重复使用 nonce")
	}
	files, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(home, f.Name()))
		if err != nil || bytes.Contains(data, []byte(secret)) {
			t.Fatal("磁盘明文", err)
		}
		info, err := f.Info()
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	second[len(second)-1] ^= 1
	if err := os.WriteFile(filepath.Join(home, "credentials.enc"), second, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := v.Get("demo", "default"); ok || err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("篡改未被拒绝")
	}
	if err := v.Set("demo", "other", "next"); err == nil {
		t.Fatal("覆盖了损坏的数据")
	}
}

func TestConcurrentProcesses(t *testing.T) {
	if dir := os.Getenv("CRAFT_TEST_VAULT_DIR"); dir != "" {
		v := newVault(dir, &fakeRing{down: true})
		if err := v.Set("demo", os.Getenv("CRAFT_TEST_ENTRY"), "example"); err != nil {
			t.Fatal(err)
		}
		return
	}
	home := t.TempDir()
	var wg sync.WaitGroup
	for n := range 8 {
		wg.Go(func() {
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestConcurrentProcesses$")
			cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "CRAFT_TEST_VAULT_DIR="+home, fmt.Sprintf("CRAFT_TEST_ENTRY=p%d", n))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("子进程: %v %s", err, out)
			}
		})
	}
	wg.Wait()
	_, entries, err := newVault(home, &fakeRing{down: true}).List()
	if err != nil || len(entries) != 8 {
		t.Fatal(entries, err)
	}
}

func TestInvalidNames(t *testing.T) {
	v := newVault(t.TempDir(), &fakeRing{})
	for _, bad := range []string{"", "a/b", "bad\n", "a b", strings.Repeat("a", 65)} {
		if err := v.Set(bad, "default", "private"); err == nil {
			t.Fatal("接受非法 provider")
		}
		if _, _, err := v.Get("ok", bad); err == nil {
			t.Fatal("接受非法 name")
		}
		if err := v.Delete("ok", bad); err == nil {
			t.Fatal("删除接受非法 name")
		}
	}
}
