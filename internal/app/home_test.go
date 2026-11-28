package app

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveStateDir(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	for _, tc := range []struct {
		name, override, want string
	}{
		{"override", filepath.Join(root, "custom"), filepath.Join(root, "custom")},
		{"relative", "relative", filepath.Join(root, "relative")},
		{"default", "", filepath.Join(root, ".craft")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(key string) string {
				if key != "CRAFT_HOME" {
					t.Fatalf("读取了意外的环境变量 %q", key)
				}
				return tc.override
			}
			dir, err := resolveStateDir(getenv, func() (string, error) {
				if tc.override != "" {
					t.Fatal("指定 CRAFT_HOME 时仍然查找了用户目录")
				}
				return root, nil
			})
			if err != nil || dir != tc.want {
				t.Fatalf("状态目录 = %q, %v，期望 %q", dir, err, tc.want)
			}
			info, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if !info.IsDir() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
				t.Errorf("状态目录模式 = %v", info.Mode())
			}
		})
	}
}
