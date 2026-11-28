package app

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

type actionHosts struct {
	owners map[string]string
	hosts  map[string]*exthost.Host
}

func (h actionHosts) host(action string) (*exthost.Host, error) {
	host := h.hosts[h.owners[action]]
	if host == nil {
		return nil, fmt.Errorf("动作 %q 没有就绪的扩展: %w", action, exthost.ErrUnavailable)
	}
	return host, nil
}

// Execute 只按已装载的归属转发，断连错误保留给状态机核实。
func (h actionHosts) Execute(ctx context.Context, p protocol.ActionExecuteParams) (outbound.Reply, error) {
	host, err := h.host(p.Action)
	if err != nil {
		return outbound.Reply{}, err
	}
	raw, err := host.ActionExecute(ctx, p)
	if err != nil {
		if remote, ok := errors.AsType[*protocol.Error](err); ok && remote.Code == protocol.ActionDenied {
			return outbound.Reply{}, fmt.Errorf("%s: %w", remote.Message, outbound.ErrDenied)
		}
		return outbound.Reply{}, err
	}
	// 结果整体原样保留；预览另取用于展示、点名和内容检查的字段。
	reply := outbound.Reply{Value: raw}
	if p.Preview {
		var preview struct {
			Summary string `json:"summary"`
			Target  string `json:"target"`
		}
		if err := jsonv2.Unmarshal(raw, &preview); err != nil {
			return outbound.Reply{}, fmt.Errorf("解码动作 %q 的预览: %w", p.Action, err)
		}
		reply.Summary, reply.Target = preview.Summary, preview.Target
	}
	return reply, nil
}

// Verify 要求扩展提供明确的核实状态；缺失和异常都不能证明未生效。
func (h actionHosts) Verify(ctx context.Context, p protocol.ActionVerifyParams) (outbound.Verification, json.RawMessage, error) {
	host, err := h.host(p.Action)
	if err != nil {
		return "", nil, err
	}
	raw, err := host.ActionVerify(ctx, p)
	if err != nil {
		return "", nil, err
	}
	var v struct {
		Status  outbound.Verification `json:"status"`
		Receipt json.RawMessage       `json:"receipt"`
	}
	if err := jsonv2.Unmarshal(raw, &v); err != nil {
		return "", nil, fmt.Errorf("解码核实结果: %w", err)
	}
	switch v.Status {
	case outbound.Applied, outbound.NotApplied, outbound.Inconclusive:
		return v.Status, v.Receipt, nil
	default:
		return "", nil, fmt.Errorf("扩展返回了无效的核实状态 %q", v.Status)
	}
}

func loadOutboundRules(path string) (map[string]outbound.Level, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// 悬空链接说明规则曾被配置，不能按首次安装处理。
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	var cfg struct {
		Organization map[string]outbound.Level `json:"organization"`
	}
	if err := jsonv2.Unmarshal(raw, &cfg, jsonv2.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("读取 %s: %w", path, err)
	}
	if cfg.Organization == nil {
		return nil, fmt.Errorf("%s 缺少 organization 对象", path)
	}
	return cfg.Organization, nil
}

func (d *Daemon) startOutbound(ctx context.Context, exts []exthost.Extension) error {
	path := filepath.Join(d.cfg.StateDir, "outbound.json")
	rules, err := loadOutboundRules(path)
	if err != nil {
		return err
	}
	hosts := actionHosts{owners: map[string]string{}, hosts: d.hosts}
	var actions []outbound.Action
	for _, e := range exts {
		for _, a := range e.Manifest.Actions {
			hosts.owners[a.ID] = e.Manifest.ID
			actions = append(actions, outbound.Action{ID: a.ID, Extension: e.Manifest.ID, Level: outbound.Level(a.Level), Reversible: a.Reversible, ReadOnly: a.ReadOnly})
		}
	}
	box := sqliteOutbox{db: d.db, repo: d.runtime.repo, health: d.health}
	d.outbound, err = outbound.New(outbound.Config{
		Outbox: box, Executor: hosts, Actions: actions, Organization: rules,
		Logger: d.cfg.Logger, Clock: d.cfg.Clock.Now, Rate: outbound.RateLimit{Max: 5, Window: time.Minute},
	})
	if err != nil {
		return fmt.Errorf("装载外发登记与 %s: %w", path, err)
	}
	return d.loadOutboxCounts(ctx)
}

func (d *Daemon) loadOutboxCounts(ctx context.Context) error {
	if err := d.db.View(ctx, func(r store.SQLiteReader) error {
		rows, err := r.Lookup("outboxCounts")
		if err != nil {
			return err
		}
		for _, row := range rows {
			var n int
			if err := jsonv2.Unmarshal(row.Value, &n); err != nil {
				return err
			}
			d.health.outboxChange("", outbound.State(row.Key), n)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("读取外发状态计数: %w", err)
	}
	return nil
}

// Outbound 返回本次装载的外发服务，仅在 Start 成功后使用。
func (d *Daemon) Outbound() *outbound.Service { return d.outbound }

func (h extensionRoutes) recentReceipts(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		After int64 `json:"afterSentSeq"`
		Limit int   `json:"limit"`
	}
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if p.After < 0 || p.Limit <= 0 {
		return nil, invalid("回执游标或条数无效")
	}
	items := []json.RawMessage{}
	err := h.db.View(ctx, func(r store.SQLiteReader) error {
		rows, err := r.Lookup("outboxReceipts", h.id, p.After, p.Limit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			items = append(items, row.Value)
		}
		return nil
	})
	return map[string]any{"items": items}, err
}
