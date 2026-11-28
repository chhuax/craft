package app

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
)

type manifestScope map[string]map[string]bool

// Scope 在命令事务里只查首次装载的声明。
func (s manifestScope) Scope(source, _ string, _ map[string]json.RawMessage) engine.ScopeResult {
	if _, ok := s[source]; ok {
		return engine.ScopeResult{Listened: true}
	}
	return engine.ScopeResult{Reason: engine.ReasonUnknownSource}
}

// Closable 依据对象类型的声明决定能否建立自有 Follow。
func (s manifestScope) Closable(source, kind string) bool { return s[source][kind] }

func daemonCatalog(exts []exthost.Extension) (manifestScope, []engine.EventDef) {
	scope := manifestScope{exthost.ReservedSource: {}}
	var defs []engine.EventDef
	for _, e := range exts {
		for _, s := range e.Manifest.WatchSources {
			kinds := scope[s.ID]
			if kinds == nil {
				kinds = map[string]bool{}
				scope[s.ID] = kinds
			}
			for _, k := range s.ThreadKinds {
				kinds[k.ID] = k.Closable
			}
			for _, ev := range s.Events {
				def := engine.EventDef{
					Source: s.ID, Event: ev.Event, Label: ev.Label,
					Default: engine.Mode(ev.Default), Strength: engine.Strength(ev.Strength),
					FollowupOnly: ev.FollowupOnly, OnUnmatched: ev.OnUnmatched,
					FeedTier: ev.FeedTier, Verb: ev.Verb, NoTarget: ev.NoTarget,
				}
				if ev.Rule != nil {
					def.Rule = &engine.EventRule{Name: ev.Rule.Name, Instruction: ev.Rule.Instruction}
				}
				if ev.Collapse != nil {
					def.Collapse = &engine.Collapse{Group: ev.Collapse.Group, By: ev.Collapse.By}
				}
				defs = append(defs, def)
			}
		}
	}
	return scope, defs
}

func loadDaemonSettings(ctx context.Context, db *store.SQLite) (engine.Settings, error) {
	var prev savedPolicy
	err := db.View(ctx, func(r store.SQLiteReader) error {
		raw, ok, err := r.Get("watch_kv", "policy.settings")
		if err != nil || !ok {
			return err
		}
		if err := jsonv2.Unmarshal(raw, &prev); err != nil {
			return fmt.Errorf("读取已生效的策略: %w", err)
		}
		return nil
	})
	return prev.Settings, err
}

// 只枚举目录项及 manifest 元数据。这样新增、移除、修复被拒绝的扩展也能触发换代。
func (d *Daemon) fingerprint() string {
	var manifests []exthost.Extension
	seen := map[string]bool{}
	for _, dir := range d.cfg.ExtensionDirs {
		root, err := filepath.Abs(dir)
		if err != nil {
			return ""
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		entries, err := os.ReadDir(root)
		if err != nil {
			return ""
		}
		for _, entry := range entries {
			if entry.IsDir() {
				manifests = append(manifests, exthost.Extension{ManifestPath: filepath.Join(root, entry.Name(), "manifest.json")})
			}
		}
	}
	return exthost.Fingerprint(d.cfg.Version, manifests)
}

func (d *Daemon) observeEffects(ctx context.Context) error {
	var stats store.EffectStats
	err := d.db.View(ctx, func(r store.SQLiteReader) error {
		var err error
		stats, err = r.EffectStats("reply")
		return err
	})
	if err == nil {
		d.health.effect("reply", HealthState{EffectStats: stats, Status: "paused"})
	}
	return err
}

func (d *Daemon) fingerprintChanged(initial string) bool {
	current := d.fingerprint()
	return current != "" && current != initial
}
