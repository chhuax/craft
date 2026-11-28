package app

import (
	"encoding/json/v2"
	"path/filepath"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

func TestPolicyRestartAndAtomicity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	ctx := t.Context()
	defs := []engine.EventDef{{Source: "feed", Event: "change", Default: engine.ModeAuto, Rule: &engine.EventRule{Name: "初版", Instruction: "逐项检查"}}}
	initial := engine.Settings{EvalTimeoutMs: 1500}
	old, err := engine.NewPolicy(initial, defs)
	checkErr(t, err)
	first := inboxDB(t, path)
	r, err := NewRuntime(first, alarm.RealClock{}, benchScope{}, nil, nil)
	checkErr(t, err)
	checkErr(t, r.Start(ctx, initial, defs))
	checkErr(t, r.repo.Atomic(ctx, func(u engine.Unit) error {
		return u.PutWork(engine.Work{ID: "work", Mandate: engine.Mandate{Strength: engine.Inferred,
			Basis: engine.MandateBasis{Source: "feed", Event: "change", Rules: []string{"feed__change"}}}})
	}))
	// 展示名不参与版本计算，故非法 UTF-8 会在引擎完成改版、编码待写入全文时才失败。
	bad := slices.Clone(defs)
	bad[0].Rule = nil
	bad[0].Label = string([]byte{0xff})
	err = r.ApplyPolicy(ctx, initial, bad)
	if err == nil {
		t.Fatal("无效设置仍然提交")
	}
	if r.policy.Load().Version() != old.Version() {
		t.Fatal("写入失败后发布了新快照")
	}
	checkErr(t, r.repo.Read(ctx, func(read engine.Reader) error {
		w, err := read.GetWork("work")
		if err != nil {
			return err
		}
		has, err := read.HasEffect("delivery", "work")
		if len(w.Inputs) != 0 || has {
			t.Fatal("设置写入失败后保留了工作输入或 effect")
		}
		return err
	}))
	checkErr(t, r.Stop(ctx))

	db := inboxDB(t, path)
	r, err = NewRuntime(db, alarm.RealClock{}, benchScope{}, nil, nil)
	checkErr(t, err)
	// 重启时用户配置和目录默认规则都变了，旧规则只能从上次保存的全文恢复。
	next := engine.Settings{Paused: true, Events: map[string]engine.EventOverride{"feed:change": {Mode: engine.ModeObserve}}}
	defs[0].Rule = nil
	p, err := engine.NewPolicy(next, defs)
	checkErr(t, err)
	checkErr(t, db.Write(ctx, func(tx *store.SQLiteTx) error { return tx.Put("watch_kv", "effect.seq", []byte(`"broken"`)) }))
	if err := r.Start(ctx, next, defs); err == nil {
		t.Fatal("投递失败仍启动了 Pass")
	}
	check := func(version string, inputs int) {
		t.Helper()
		checkErr(t, db.View(ctx, func(tx store.SQLiteReader) error {
			value, ok, err := tx.Get("watch_kv", "policy.settings")
			if !ok || err != nil {
				t.Fatalf("缺少设置 %v", err)
			}
			var doc savedPolicy
			if err := json.Unmarshal(value, &doc); err != nil {
				return err
			}
			if doc.Version != version {
				t.Fatalf("版本 %s, 期望 %s", doc.Version, version)
			}
			if version == old.Version() && (doc.Settings.EvalTimeoutMs != 1500 || len(doc.Catalog) != 1 || doc.Catalog[0].Rule == nil) {
				t.Fatalf("未持久化完整旧策略 %s", value)
			}
			u := engineReader{tx}
			w, err := u.GetWork("work")
			if err != nil {
				return err
			}
			has, err := tx.HasEffect("delivery", "work")
			if err != nil {
				return err
			}
			if len(w.Inputs) != inputs || has != (inputs != 0) {
				t.Fatalf("改版结果 %v, effect=%t", w.Inputs, has)
			}
			if inputs > 0 {
				in := w.Inputs[0]
				if in.From != old.Version() || in.To != p.Version() || in.Effect == nil || len(in.Effect.Rules) != 1 || in.Effect.Rules[0].Name != "初版" || in.Effect.Proactive == nil || *in.Effect.Proactive {
					t.Fatalf("改版输入 %+v", in)
				}
			}
			return nil
		}))
	}
	check(old.Version(), 0)
	checkErr(t, db.Write(ctx, func(tx *store.SQLiteTx) error { return tx.Delete("watch_kv", "effect.seq") }))
	checkErr(t, r.Start(ctx, next, defs))
	check(p.Version(), 1)
	// 同版的运行参数也应生效并写回全文，但不能发第二次改版输入。
	next.Paused = false
	next.EvalTimeoutMs = 42
	checkErr(t, r.ApplyPolicy(ctx, next, defs))
	if r.policy.Load().Paused() || r.policy.Load().EvalTimeoutMs() != 42 {
		t.Fatal("同版运行参数未生效")
	}
	check(p.Version(), 1)
	checkErr(t, db.View(ctx, func(tx store.SQLiteReader) error {
		raw, _, err := tx.Get("watch_kv", "policy.settings")
		if err != nil {
			return err
		}
		var saved savedPolicy
		if err := json.Unmarshal(raw, &saved); err != nil {
			return err
		}
		if saved.Settings.Paused || saved.Settings.EvalTimeoutMs != 42 {
			t.Fatal("同版设置全文未更新")
		}
		return nil
	}))
	checkErr(t, r.Stop(ctx))
}

func TestCommandPolicyConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CRAFT_HOME", home)
	clock := func() string { return "2026-01-01T00:00:00.000Z" }
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, patch := range []string{`{"paused":true}`, `{"allowedSenders":[{"source":"feed","id":"reader"}]}`} {
		c, err := OpenCommands(t.Context(), home, clock, nil)
		checkErr(t, err)
		t.Cleanup(func() {
			if err := c.Close(); err != nil {
				t.Error(err)
			}
		})
		go func() {
			<-start
			_, err := c.SetPolicy(t.Context(), []byte(patch))
			results <- err
		}()
	}
	close(start)
	for range 2 {
		checkErr(t, <-results)
	}
	err := ViewCommands(t.Context(), home, func(r CommandReader) error {
		p, err := r.Policy()
		if err == nil && (!p.Settings.Paused || len(p.Settings.AllowedSenders) != 1) {
			t.Errorf("并发覆盖了另一条命令的字段：%+v", p.Settings)
		}
		return err
	})
	checkErr(t, err)
}
