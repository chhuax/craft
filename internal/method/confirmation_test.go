package method

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

var instant = time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)

func TestVerify(t *testing.T) {
	d := Parse("changes/unit/design.md", "## 结论\n保留排序")
	c, err := Confirm("评审人", instant, d)
	if err != nil {
		t.Fatal(err)
	}
	if c.By != "评审人" || !c.At.Equal(instant) || c.Version != 1 {
		t.Fatal(c)
	}
	for _, tc := range []struct {
		doc           []Document
		state, detail string
	}{
		{[]Document{d}, "confirmed", "ok"},
		{[]Document{Parse(d.Path, "---\nstatus: ok\n---\n## 结论\r\n保留排序")}, "confirmed", "ok"},
		{[]Document{Parse(d.Path, "修改"), Parse("archive/design.md", "## 结论\n保留排序")}, "stale", "changed"},
		{[]Document{Parse("other/design.md", "其他"), Parse("archive/design.md", "## 结论\n保留排序")}, "confirmed", "moved"},
		{[]Document{Parse("other/name.md", "## 结论\n保留排序")}, "stale", "missing"},
		{nil, "stale", "missing"},
		{[]Document{Parse(d.Path, "---\ncraft: 2\n---\n## 结论\n保留排序")}, "stale", "changed"},
	} {
		got := Verify(c, tc.doc...)
		if got.State != tc.state || len(got.Docs) != 1 || got.Docs[0].State != tc.detail || tc.detail == "moved" && got.Docs[0].MovedTo != "archive/design.md" {
			t.Error(tc, got)
		}
	}
	if got := Verify(nil, d); got.State != "none" {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		by   string
		at   time.Time
		docs []Document
	}{
		{"", instant, []Document{d}}, {"人", time.Time{}, []Document{d}}, {"人", instant, nil}, {"人", instant, []Document{d, d}},
		{"人", instant, []Document{Parse(d.Path, "---\ncraft: 2\n---")}}, {"人", instant, []Document{Parse("../design.md", "文")}},
	} {
		if _, err := Confirm(tc.by, tc.at, tc.docs...); err == nil {
			t.Error(tc)
		}
	}
}

func TestFileStore(t *testing.T) {
	project := t.TempDir()
	store, err := NewFileStore(project, "")
	if err != nil {
		t.Fatal(err)
	}
	c, err := store.Load("unit")
	if c != nil || err != nil {
		t.Fatal(c, err)
	}
	if entries, err := os.ReadDir(project); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
	c, err = Confirm("人", instant, Parse("design.md", "结论"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("unit", *c); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("unit")
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatal(got, err)
	}
	dir := filepath.Join(project, ".craft", "method")
	ignore := filepath.Join(dir, ".gitignore")
	if data, err := os.ReadFile(ignore); err != nil || string(data) != "*\n" {
		t.Fatal(string(data), err)
	}
	if err := os.WriteFile(ignore, []byte("# 保留\n*\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.By = "复核人"
	if err := store.Save("unit", *c); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(ignore); err != nil || string(data) != "# 保留\n*\n" {
		t.Fatal(string(data), err)
	}
	file := filepath.Join(dir, "unit.json")
	for _, raw := range []string{`{"version":2}`, `{`, `{}`} {
		if err := os.WriteFile(file, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load("unit"); err == nil {
			t.Fatal(raw)
		}
		if err := store.Save("unit", *c); err == nil {
			t.Fatal("覆盖未知格式", raw)
		}
	}
	for _, name := range []string{"../x", `C:\dir`, `a\b`, "/a", "..", "NUL", "aux.txt", "com1", "x:stream", "a.", "a ", ".git", "LPT².dat"} {
		if _, err := NewFileStore(project, name); err == nil {
			t.Error("目录", name)
		}
		if err := store.Save(name, *c); err == nil {
			t.Error("记录名", name)
		}
	}
}

func TestStoreConfined(t *testing.T) {
	project, outside := t.TempDir(), t.TempDir()
	store, err := NewFileStore(project, ".state")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(project, ".state")); err != nil {
		t.Skipf("当前系统无法创建符号链接：%v", err)
	}
	c, err := Confirm("人", instant, Parse("design.md", "结论"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save("unit", *c); err == nil {
		t.Fatal("状态越过项目边界")
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatal(entries, err)
	}
}

func TestRetierRequiresConfirmation(t *testing.T) {
	const head = "---\ncraft: 1\ntier: standard\n---\n正文"
	d := Parse("requirements.md", head)
	c, err := Confirm("人", instant, d)
	if err != nil {
		t.Fatal(err)
	}
	other := Parse(d.Path, strings.Replace(head, "standard", "heavy", 1))
	if other.hash != d.hash || Verify(c, other).State != "stale" {
		t.Fatal(c, other)
	}
}

func TestStoreConcurrent(t *testing.T) {
	store, err := NewFileStore(t.TempDir(), ".local-state")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Go(func() {
			d := Parse("design.md", fmt.Sprint(i))
			c, err := Confirm(fmt.Sprint(i), instant, d)
			if err != nil {
				t.Error(err)
				return
			}
			if err := store.Save("unit", *c); err != nil {
				t.Error(err)
				return
			}
			got, err := store.Load("unit")
			if err != nil || got == nil || len(got.Docs) != 1 || got.Docs[0].Hash != normalizedHash(got.By) {
				t.Error(got, err)
			}
		})
	}
	wg.Wait()
}
