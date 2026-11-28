package exthost

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func manifestText(id, extra string) string {
	return fmt.Sprintf(`{"id":%q,"version":"v1","protocol":1,"exec":["./ext","--stdio"]%s}`, id, extra)
}

func putManifest(t *testing.T, root, id, data string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManifest(t *testing.T) {
	extra := `,
 "alarms":[{"id":"a","every":"2m","jitter":"1s","timeout":"5s"},{"id":"b","at":"00:00","default_enabled":false}],
 "alarm_limits":{"concurrency":2},
 "watch_sources":[{
   "id":"src","thread_kinds":[{"id":"item","closable":true,"object_type":"demo.item"}],
   "events":[{
     "id":"open","label":"待处理","default":"observe","strength":"explicit","followup_only":true,
     "on_unmatched":"record","feed_tier":"notice","no_target":"scratch","verb":"一二三四五🌿",
     "rule":{"name":"处理","instruction":"核对"},"collapse":{"group":"item","by":"kind"}
   }],
   "facts":["due"]
 }],
 "actions":[{"n":9007199254740993}],"ports":{},"placement_rules":[],"views":[],"future":{"n":1}`
	for _, tc := range []struct{ mode, strength, unmatched, tier string }{{"observe", "explicit", "record", "notice"}, {"off", "inferred", "filter", "digest"}, {"auto", "explicit", "record", "digest"}} {
		replace := strings.NewReplacer("observe", tc.mode, "explicit", tc.strength, "record", tc.unmatched, "notice", tc.tier)
		p := putManifest(t, t.TempDir(), "demo", manifestText("demo", replace.Replace(extra)))
		m, err := loadManifest(p)
		if err != nil {
			t.Fatal(err)
		}
		alarms := []AlarmDecl{{ID: "a", Every: 2 * time.Minute, Jitter: time.Second, Timeout: 5 * time.Second, DefaultEnabled: true}, {ID: "b", At: "00:00"}}
		if !slices.Equal(m.Alarms, alarms) || m.AlarmLimits.Concurrency == nil || *m.AlarmLimits.Concurrency != 2 {
			t.Fatal(m.Alarms, m.AlarmLimits)
		}
		source := WatchSource{
			ID: "src", Facts: []string{"due"},
			ThreadKinds: []ThreadKind{{ID: "item", ObjectType: "demo.item", Closable: true}},
			Events: []EventDecl{{
				Source: "src", Event: "open", Label: "待处理", Default: tc.mode, Strength: tc.strength, FollowupOnly: true,
				OnUnmatched: tc.unmatched, FeedTier: tc.tier, Verb: "一二三四五🌿", NoTarget: "scratch",
				Rule: &EventRule{Name: "处理", Instruction: "核对"}, Collapse: &Collapse{Group: "item", By: "kind"},
			}},
		}
		if !reflect.DeepEqual(m.WatchSources, []WatchSource{source}) {
			t.Fatal(m.WatchSources)
		}
		if string(m.Actions) != `[{"n":9007199254740993}]` || string(m.Ports) != "{}" || string(m.PlacementRules) != "[]" || string(m.Views) != "[]" {
			t.Fatal(m)
		}
	}
}

func TestRejectManifest(t *testing.T) {
	for _, tc := range []struct{ field, from, to string }{
		{"id", `"id":"demo"`, `"id":""`},
		{"version", `"v1"`, `""`},
		{"protocol", `"protocol":1`, `"protocol":"1"`},
		{"protocol", `"protocol":1`, `"protocol":1.1`},
		{"exec", `["./ext","--stdio"]`, `[]`},
		{"exec", "./ext", "../ext"},
		{"exec", "./ext", "/ext"},
		{"alarms", `"every":"1m"`, `"every":"1m","at":"00:00"`},
		{"alarms", `,"every":"1m"`, ""},
		{"every", "1m", "0s"},
		{"at", `"every":"1m"`, `"at":"25:00"`},
		{"jitter", `"every":"1m"`, `"every":"1m","jitter":"bad"`},
		{"timeout", `"every":"1m"`, `"every":"1m","timeout":"bad"`},
		{"jitter", `"every":"1m"`, `"every":"1m","jitter":"-1s"`},
		{"timeout", `"every":"1m"`, `"every":"1m","timeout":"-1s"`},
		{"concurrency", `"concurrency":1`, `"concurrency":0`},
		{"default", "observe", "bad"},
		{"strength", "inferred", "bad"},
		{"on_unmatched", "filter", "bad"},
		{"feed_tier", "digest", "bad"},
		{"verb", "处理", "一二三四五六七"},
		{"no_target", "scratch", "other"},
		{"group", `"group":"g"`, `"group":""`},
		{"by", `"by":"k"`, `"by":3`},
		{"保留名", `"id":"src"`, `"id":"craft"`},
	} {
		t.Run(tc.field, func(t *testing.T) {
			data := manifestText("demo", `,"alarms":[{"id":"a","every":"1m"}],"alarm_limits":{"concurrency":1},"watch_sources":[{"id":"src","events":[{"id":"e","default":"observe","strength":"inferred","on_unmatched":"filter","feed_tier":"digest","verb":"处理","no_target":"scratch","collapse":{"group":"g","by":"k"}}]}]`)
			data = strings.Replace(data, tc.from, tc.to, 1)
			p := putManifest(t, t.TempDir(), "demo", data)
			_, err := loadManifest(p)
			if err == nil || !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("字段 %s：%v", tc.field, err)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	event := `,"watch_sources":[{"id":"src","events":[{"id":"e","default":"auto","strength":"explicit"}]}]`
	for _, tc := range []struct{ id, extra string }{
		{"a", `,"requires":["z"]`}, {"z", ""},
		{"cycle", `,"requires":["loop"]`}, {"loop", `,"requires":["cycle"]`},
		{"missing", `,"requires":["absent"]`}, {"blocked", `,"requires":["missing"]`},
		{"dup", ""}, {"first", event}, {"second", event}, {"dependent", `,"requires":["first"]`},
	} {
		putManifest(t, a, tc.id, manifestText(tc.id, tc.extra))
	}
	putManifest(t, b, "dup", manifestText("dup", ""))
	putManifest(t, b, "wrong", manifestText("other", ""))
	putManifest(t, b, "bad", "{")
	r := Discover(a, b)
	var ids []string
	for _, e := range r.Extensions {
		ids = append(ids, e.Manifest.ID)
		if !filepath.IsAbs(e.Dir) || e.ManifestPath != filepath.Join(e.Dir, "manifest.json") {
			t.Fatal(e)
		}
	}
	if !slices.Equal(ids, []string{"z", "a"}) || len(r.Rejected) != 11 {
		t.Fatalf("装载 %v，拒绝 %v", ids, r.Rejected)
	}
	for _, id := range []string{"cycle", "loop"} {
		err := r.Rejected[filepath.Join(a, id, "manifest.json")]
		if err == nil || !strings.Contains(err.Error(), "cycle -> loop -> cycle") {
			t.Fatalf("缺环路径：%v", err)
		}
	}
}

func TestFingerprint(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"a", "b"} {
		putManifest(t, root, id, manifestText(id, ""))
	}
	exts := Discover(root).Extensions
	original := slices.Clone(exts)
	f := Fingerprint("1", exts)
	if f == "" || Fingerprint("1", exts) != f || Fingerprint("2", exts) == f {
		t.Fatal("版本或稳定性")
	}
	slices.Reverse(exts)
	if Fingerprint("1", exts) != f || !reflect.DeepEqual(exts[0], original[1]) {
		t.Fatal("顺序影响指纹")
	}
	p := exts[0].ManifestPath
	if err := os.WriteFile(filepath.Join(exts[0].Dir, "ext"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if Fingerprint("1", exts) != f {
		t.Fatal("读取了扩展代码")
	}
	ts := time.Unix(12345, 0)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
	if Fingerprint("1", exts) == f {
		t.Fatal("忽略了修改时间")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if Fingerprint("1", exts) != "" {
		t.Fatal("缺文件时触发重载")
	}
}

func TestManifestFieldNamesAndTypes(t *testing.T) {
	for _, tc := range []struct{ field, extra string }{
		{"exec", `,"exec":["./ext",null]`},
		{"facts", `,"watch_sources":[{"id":"src","facts":[null]}]`},
		{"default_enabled", `,"alarms":[{"id":"a","every":"1s","default_enabled":null}]`},
		{"verb", `,"watch_sources":[{"id":"s","events":[{"id":"e","default":"off","strength":"inferred","verb":null}]}]`},
		{"by", `,"watch_sources":[{"id":"s","events":[{"id":"e","default":"off","strength":"inferred","collapse":{"group":"g","by":null}}]}]`},
		{"concurrency", `,"alarm_limits":{"concurrency":null}`},
	} {
		data := manifestText("demo", tc.extra)
		if tc.field == "exec" {
			data = strings.Replace(data, `,"exec":["./ext","--stdio"]`, "", 1)
		}
		p := putManifest(t, t.TempDir(), "demo", data)
		_, err := loadManifest(p)
		if err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s 接受了错误类型或缺少字段路径: %v", tc.field, err)
		}
	}
	p := putManifest(t, t.TempDir(), "demo", manifestText("demo", `,"VERSION":3`))
	m, err := loadManifest(p)
	if err != nil || m.Version != "v1" {
		t.Fatal("未知字段覆盖了 version", err)
	}
}

func TestDuplicateEventWithinManifest(t *testing.T) {
	root := t.TempDir()
	event := `{"id":"e","default":"auto","strength":"explicit"}`
	source := func(events string) string { return `,"watch_sources":[{"id":"s","events":[` + events + `]}]` }
	putManifest(t, root, "bad", manifestText("bad", source(event+","+event)))
	putManifest(t, root, "good", manifestText("good", source(event)))
	r := Discover(root)
	if len(r.Extensions) != 1 || r.Extensions[0].Manifest.ID != "good" || len(r.Rejected) != 1 {
		t.Fatalf("文件内重复项影响独立扩展: %+v", r)
	}
}

func TestFingerprintSizeAndPath(t *testing.T) {
	root := t.TempDir()
	p := putManifest(t, root, "one", manifestText("one", ""))
	exts := Discover(root).Extensions
	ts := time.Unix(1000, 0)
	stamp := func(path string) {
		t.Helper()
		if err := os.Chtimes(path, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	stamp(p)
	first := Fingerprint("v", exts)
	other := putManifest(t, t.TempDir(), "one", manifestText("one", ""))
	stamp(other)
	exts[0].ManifestPath = other
	if Fingerprint("v", exts) == first {
		t.Fatal("忽略了路径")
	}
	exts[0].ManifestPath = p
	if err := os.WriteFile(p, []byte(manifestText("one", "")+" "), 0600); err != nil {
		t.Fatal(err)
	}
	stamp(p)
	if Fingerprint("v", exts) == first {
		t.Fatal("相同修改时间时忽略了大小")
	}
}

func TestDiscoverMixedDependencyErrors(t *testing.T) {
	root := t.TempDir()
	p := putManifest(t, root, "a", manifestText("a", `,"requires":["missing","b"]`))
	putManifest(t, root, "b", manifestText("b", `,"requires":["a"]`))
	putManifest(t, root, "ok", manifestText("ok", ""))
	r := Discover(root, root)
	err := r.Rejected[p]
	if len(r.Extensions) != 1 || r.Extensions[0].Manifest.ID != "ok" || err == nil || !strings.Contains(err.Error(), "missing") || !strings.Contains(err.Error(), "a -> b -> a") {
		t.Fatalf("装载 %v，拒绝 %v", r.Extensions, r.Rejected)
	}
}
