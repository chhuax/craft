package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
)

func TestDaemonExtensionWrites(t *testing.T) {
	r := testRuntime(t, inboxDB(t, filepath.Join(t.TempDir(), "db")))
	h, err := newExtensionHandler(feedManifest(), r, newDaemonHealth(nil))
	checkErr(t, err)
	ctx := t.Context()
	<-r.wake // 清除夹具应用初始策略的提交唤醒。
	for _, tc := range []struct{ method, params string }{
		{"thread/describe", `{"key":"foreign:item:x"}`}, {"thread/describe", `{"key":"feed:item:x","ref":{"source":"foreign","kind":"item","id":"x"}}`},
		{"thread/patch", `{"key":"foreign:item:x","meta":{}}`}, {"thread/closed", `{"thread":{"source":"foreign","kind":"item","id":"x"},"at":"2026-01-01T00:00:00Z","outcome":"done"}`},
		{"thread/reopened", `{"thread":{"source":"foreign","kind":"item","id":"x"},"at":"2026-01-01T00:00:00Z"}`}, {"work/bindThread", `{"workId":"w","thread":{"source":"feed","kind":"item:x","id":"y"},"own":true}`},
		{"receipts/recent", `{}`}, {"receipts/recent", `{"limit":2,"afterSentSeq":-1}`},
		{"health/report", `{"entry":"e","state":"ready"}`}, {"thread/closed", `{"thread":{"source":"feed","kind":"item","id":"x"},"outcome":"other"}`},
	} {
		_, err := h(ctx, tc.method, []byte(tc.params))
		if !errors.Is(err, exthost.ErrInvalidParams) {
			t.Errorf("%s %s: %v", tc.method, tc.params, err)
		}
	}
	select {
	case <-r.wake:
		t.Fatal("被拒绝的请求唤醒了 Pass")
	default:
	}
	_, err = h(ctx, "unknown", nil)
	if !errors.Is(err, exthost.ErrMethodNotFound) {
		t.Fatal(err)
	}
	_, err = h(ctx, "thread/describe", []byte(`{"key":"feed:item:x:y","label":"first"}`))
	checkErr(t, err)
	select {
	case <-r.wake:
	default:
		t.Fatal("描述提交后未唤醒 Pass")
	}
	_, err = h(ctx, "thread/patch", []byte(`{"key":"feed:item:x:y","meta":{"label":"second"}}`))
	checkErr(t, err)
	got, err := h(ctx, "thread/get", []byte(`{"key":"feed:item:x:y"}`))
	checkErr(t, err)
	if th := got.(*engine.Thread); th.Ref.ID() != "x:y" || th.Meta.Label != "second" {
		t.Fatal(th)
	}
}

func TestDaemonStateQuotaAndNamespace(t *testing.T) {
	r := testRuntime(t, inboxDB(t, filepath.Join(t.TempDir(), "db")))
	m := feedManifest()
	h, err := newExtensionHandler(m, r, newDaemonHealth(nil))
	checkErr(t, err)
	m.ID = "another"
	other, err := newExtensionHandler(m, r, newDaemonHealth(nil))
	checkErr(t, err)
	set := func(key, value string) error {
		_, err := h(t.Context(), "state/set", []byte(fmt.Sprintf(`{"key":%q,"value":%q}`, key, value)))
		return err
	}
	if !errors.Is(set("large", strings.Repeat("x", 4<<10)), exthost.ErrInvalidParams) {
		t.Fatal("大值未拒绝")
	}
	for i := range exthost.MaxStateKeys {
		checkErr(t, set(fmt.Sprint(i), "cursor"))
	}
	if !errors.Is(set("overflow", "x"), exthost.ErrInvalidParams) {
		t.Fatal("键数越限")
	}
	checkErr(t, set("0", "changed"))
	for _, tc := range []struct {
		handler exthost.Handler
		keys    int
		value   string
	}{{other, 0, "null"}, {h, exthost.MaxStateKeys, `"changed"`}} {
		handler := tc.handler
		v, err := handler(t.Context(), "state/get", []byte(`{"key":"0"}`))
		checkErr(t, err)
		keys, err := handler(t.Context(), "state/keys", nil)
		checkErr(t, err)
		raw, err := json.Marshal(v)
		checkErr(t, err)
		if len(keys.([]string)) != tc.keys || string(raw) != tc.value {
			t.Fatal(keys, string(raw))
		}
	}
}

func TestSubscribedRetention(t *testing.T) { synctest.Test(t, testSubscribedRetention) }

func testSubscribedRetention(t *testing.T) {
	r := testRuntime(t, inboxDB(t, filepath.Join(t.TempDir(), "db")))
	h, err := newExtensionHandler(feedManifest(), r, newDaemonHealth(nil))
	checkErr(t, err)
	now := r.clock.Now()
	old := now.Add(-15 * 24 * time.Hour).UTC().Format(time.RFC3339)
	recent := now.Add(-13 * 24 * time.Hour).UTC().Format(time.RFC3339)
	checkErr(t, r.repo.Atomic(t.Context(), func(u engine.Unit) error {
		for _, w := range []engine.Work{{ID: "open"}, {ID: "ended", Ended: &engine.WorkEnd{At: old}}, {ID: "recent", Ended: &engine.WorkEnd{At: recent}}} {
			if err := u.PutWork(w); err != nil {
				return err
			}
		}
		for _, tc := range []struct {
			id, src, kind, last string
			subs                []string
		}{
			{"live", "feed", "item", old, []string{"open"}}, {"ended", "feed", "item", old, []string{"ended", "recent"}},
			{"new", "feed", "item", recent, []string{"ended"}}, {"expired", "feed", "item", old, []string{"ended"}},
			{"empty", "feed", "item", recent, nil}, {"foreign", "other", "item", recent, []string{"open"}}, {"side", "side", "note", recent, []string{"open"}},
		} {
			ref := engine.NewThreadRef(tc.src, tc.kind, tc.id)
			if err := u.PutThread(engine.Thread{Ref: ref, Key: engine.ThreadKey(ref), CreatedAt: tc.last, Subscriptions: tc.subs}); err != nil {
				return err
			}
		}
		return nil
	}))
	got, err := h(t.Context(), "thread/subscribed", []byte(`{"kind":"item"}`))
	checkErr(t, err)
	var ids []string
	for _, th := range got.([]engine.Thread) {
		ids = append(ids, th.Ref.ID())
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []string{"ended", "live", "new"}) {
		t.Fatal(ids)
	}
}
