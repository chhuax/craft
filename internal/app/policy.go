package app

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

// 重启时目录中的默认模式和规则可能已更新，仅保存覆盖项无法还原旧授权。
type savedPolicy struct {
	Version  string            `json:"version"`
	Settings engine.Settings   `json:"settings"`
	Catalog  []engine.EventDef `json:"catalog"`
}

const policyKey = "policy.settings"

func (r CommandReader) policy() (savedPolicy, engine.Policy, error) {
	var saved savedPolicy
	if r.read != nil {
		raw, ok, err := r.read.Get("watch_kv", policyKey)
		if err != nil {
			return saved, engine.Policy{}, err
		}
		if ok {
			if err := json.Unmarshal(raw, &saved); err != nil {
				return saved, engine.Policy{}, fmt.Errorf("解码上次策略: %w", err)
			}
			if saved.Version == "" {
				return saved, engine.Policy{}, errors.New("已保存策略缺少版本")
			}
		}
	}
	p, err := engine.NewPolicy(saved.Settings, saved.Catalog)
	if err != nil {
		return saved, p, fmt.Errorf("还原上次策略: %w", err)
	}
	if saved.Version != "" && saved.Version != p.Version() {
		return saved, p, errors.New("已保存策略的版本与全文不符")
	}
	return saved, p, nil
}

func savePolicy(tx *store.SQLiteTx, saved savedPolicy) (bool, error) {
	value, err := json.Marshal(saved, json.Deterministic(true))
	if err != nil {
		return false, fmt.Errorf("编码待生效策略: %w", err)
	}
	raw, _, err := tx.Get("watch_kv", policyKey)
	if err != nil || bytes.Equal(raw, value) {
		return false, err
	}
	err = tx.Put("watch_kv", policyKey, value)
	return err == nil, err
}

// ApplyPolicy 在同一事务内更新受影响的工作、研判和完整设置，失败时可重试。
func (r *Runtime) ApplyPolicy(ctx context.Context, settings engine.Settings, catalog []engine.EventDef) error {
	return r.applyPolicy(ctx, func(savedPolicy) (engine.Settings, []engine.EventDef, error) { return settings, catalog, nil })
}

func (r *Runtime) applyPolicy(ctx context.Context, change func(savedPolicy) (engine.Settings, []engine.EventDef, error)) error {
	var next engine.Policy
	err := r.repo.db.WriteAndPublish(ctx, func(tx *store.SQLiteTx) error {
		prev, old, err := (CommandReader{&tx.SQLiteReader}).policy()
		if err != nil {
			return err
		}
		settings, catalog, err := change(prev)
		if err != nil {
			return err
		}
		next, err = engine.NewPolicy(settings, catalog)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSettings, err)
		}
		if prev.Version != "" && prev.Version != next.Version() {
			if err := r.pass.PolicyChanged(ctx, old, next, r.repo.in(ctx, tx)); err != nil {
				return err
			}
		}
		changed, err := savePolicy(tx, savedPolicy{next.Version(), settings, catalog})
		if changed {
			tx.AfterCommit(r.Request)
		}
		return err
	}, func() { r.policy.Store(&next) })
	if err != nil {
		return fmt.Errorf("应用策略: %w", err)
	}
	return nil
}

// refreshPolicy 把别的进程（例如 CLI 的 `policy set`）已经提交的策略发布进本进程的快照。
//
// 为什么需要：策略快照只存在于内存里，CLI 在自己的进程里改设置并提交，daemon 的 data_version
// 轮询只会唤醒 Pass，不会重读策略；不重读的话 daemon 会一直按旧策略准入，直到重启。
//
// 为什么用「不写任何东西的写事务」而不是只读事务：发布必须和本进程自己的 ApplyPolicy 的发布按提交顺序
// 串行，否则这里读到旧版、却在新版发布之后才存进去，快照就回退了。WriteAndPublish 的 publish 在提交后、
// 仍持有写连接时执行，天然串行；事务里没有写入，所以没有 fsync，开销只是一次加锁。
// 库里还没有已保存的策略时什么也不发布，以免用默认策略覆盖启动时装入的目录。
func (r *Runtime) refreshPolicy(ctx context.Context) error {
	var next engine.Policy
	var found bool
	err := r.repo.db.WriteAndPublish(ctx, func(tx *store.SQLiteTx) error {
		saved, p, err := (CommandReader{&tx.SQLiteReader}).policy()
		next, found = p, saved.Version != ""
		return err
	}, func() {
		if found {
			r.policy.Store(&next)
		}
	})
	if err != nil {
		return fmt.Errorf("重读策略: %w", err)
	}
	return nil
}

// ErrInvalidSettings 区分无效的用户设置与数据库故障。
var ErrInvalidSettings = errors.New("设置参数无效")

// EffectivePolicy 是某一已提交快照内实际参与判定的模式、规则及版本。
type EffectivePolicy struct {
	Version  string          `json:"version"`
	Settings engine.Settings `json:"settings"`
}

func effective(saved savedPolicy, p engine.Policy) EffectivePolicy {
	s := saved.Settings
	s.Rules = p.EffectiveRules()
	s.Events = map[string]engine.EventOverride{}
	for _, d := range saved.Catalog {
		s.Events[d.Source+":"+d.Event] = engine.EventOverride{Mode: p.ModeOf(d.Source, d.Event)}
	}
	for _, event := range []string{"person.request", "person.direct", "person.answer"} {
		s.Events["craft:"+event] = engine.EventOverride{Mode: p.ModeOf("craft", event)}
	}
	return EffectivePolicy{p.Version(), s}
}

// Policy 读取生效规则，未初始化的库返回引擎默认策略。
func (r CommandReader) Policy() (EffectivePolicy, error) {
	s, p, err := r.policy()
	if err != nil {
		return EffectivePolicy{}, err
	}
	return effective(s, p), nil
}

func patchSettings(prev savedPolicy, raw []byte) (engine.Settings, []engine.EventDef, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return prev.Settings, nil, fmt.Errorf("%w: %w", ErrInvalidSettings, err)
	}
	if len(fields) == 0 {
		return prev.Settings, nil, fmt.Errorf("%w: 需要指定设置字段", ErrInvalidSettings)
	}
	s := prev.Settings
	opts := json.WithUnmarshalers(json.UnmarshalFromFunc(func(d *jsontext.Decoder, _ any) error {
		if d.PeekKind() == 'n' {
			return errors.New("设置不能为 null")
		}
		return errors.ErrUnsupported
	}))
	for k, v := range fields {
		var dest any
		switch k {
		case "paused":
			dest = &s.Paused
		case "allowedSenders":
			dest = &s.AllowedSenders
		case "rules":
			dest = &s.Rules
		case "events":
			s.Events = nil
			dest = &s.Events
		default:
			return s, nil, fmt.Errorf("%w: 未知字段 %q", ErrInvalidSettings, k)
		}
		if err := json.Unmarshal(v, dest, json.RejectUnknownMembers(true), opts); err != nil {
			return s, nil, fmt.Errorf("%w: %s: %w", ErrInvalidSettings, k, err)
		}
	}
	if _, ok := fields["events"]; ok {
		known := map[string]bool{}
		for _, d := range prev.Catalog {
			known[d.Source+":"+d.Event] = true
		}
		for k := range s.Events {
			if !known[k] || strings.HasPrefix(k, "craft:") {
				return s, nil, fmt.Errorf("%w: 事件 %q 不能配置", ErrInvalidSettings, k)
			}
		}
	}
	return s, prev.Catalog, nil
}

// SetPolicy 在 ApplyPolicy 共用的写事务内合并传入字段，避免两个 CLI 互相覆盖设置。
func (c *Commands) SetPolicy(ctx context.Context, raw []byte) (EffectivePolicy, error) {
	repo := &engineStore{db: c.db}
	e, err := engine.New(engine.Config{Store: repo, Scope: c.scope, Clock: c.clock, Policy: func() engine.Policy { return engine.Policy{} }})
	if err != nil {
		return EffectivePolicy{}, err
	}
	r := &Runtime{pass: e, repo: repo}
	var saved savedPolicy
	err = r.applyPolicy(ctx, func(prev savedPolicy) (engine.Settings, []engine.EventDef, error) {
		s, cat, err := patchSettings(prev, raw)
		saved = savedPolicy{Settings: s, Catalog: cat}
		return s, cat, err
	})
	if err != nil {
		return EffectivePolicy{}, err
	}
	return effective(saved, *r.policy.Load()), nil
}
