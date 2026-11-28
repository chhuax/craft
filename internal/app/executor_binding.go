package app

import (
	"context"
	"fmt"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

// SessionFromEnv 优先使用 Claude Code 的原生会话号，后台服务继承的派工环境可能属于另一项 Work。
// 其余宿主沿用派工声明，声明不全时保留绑定，以免启动中的执行方获得人的权限。
func SessionFromEnv(getenv func(string) string) *SessionBinding {
	if id := getenv(host.EnvClaudeSessionID); id != "" && getenv(host.EnvCodexThreadID) == "" {
		return &SessionBinding{Session: engine.Session{Host: host.ClaudeCode, ID: id}}
	}
	name, id, work, assignment, role := getenv(host.EnvHost), getenv(host.EnvSessionID), getenv(host.EnvWork), getenv(host.EnvAssignment), getenv(host.EnvRole)
	if name == "" && id == "" && work == "" && assignment == "" && role == "" {
		return nil
	}
	if name == "codex" {
		native := getenv(host.EnvCodexThreadID)
		if native != "" {
			if id != "" && id != native {
				return &SessionBinding{}
			}
			id = native
		}
	}
	if role != "worker" || name == "" || work == "" || assignment == "" {
		return &SessionBinding{}
	}
	return &SessionBinding{WorkID: work, Assignment: assignment, Session: engine.Session{Host: name, ID: id}}
}

// NeedsResolution 区分原生会话身份与派工声明，后者即使残缺也不能借此获取权限。
func (b *SessionBinding) NeedsResolution() bool {
	return b != nil && b.Session.Host == host.ClaudeCode && b.Session.ID != "" && b.WorkID == "" && b.Assignment == ""
}

// BindingForSession 根据只读快照识别角色，执行命令时仍须在事务内核对派工代次。
func BindingForSession(ctx context.Context, home string, s engine.Session) (*SessionBinding, error) {
	var b *SessionBinding
	err := ViewCommands(ctx, home, func(r CommandReader) error {
		w, err := r.sessionWork(s)
		if err != nil || w == nil || w.Ended != nil {
			return err
		}
		a := latest(w)
		if a == nil || a.ID == "" {
			return &engine.UserError{Code: engine.CodeUnknownAssignment}
		}
		b = &SessionBinding{WorkID: w.ID, Session: s, Assignment: a.ID}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("解析宿主会话 %q 的派工: %w", s.ID, err)
	}
	return b, nil
}

// WorkerTurnEnded 处理宿主 Stop 通知，已登记但投递尚未确认时，保留信号等送达事务消费。
func (c *Commands) WorkerTurnEnded(ctx context.Context, b SessionBinding) error {
	return c.db.Write(ctx, func(tx *store.SQLiteTx) error {
		rd := CommandReader{&tx.SQLiteReader}
		w, err := rd.boundWork(b)
		if err != nil {
			return err
		}
		repo := &engineStore{db: c.db}
		u := repo.in(ctx, tx)
		s := host.Session(b.Session)
		a := latest(w)
		pending, err := pendingRound(u, a.ID)
		if err != nil {
			return err
		}
		if pending || a.State != engine.AssignmentRunning {
			return u.put("watch_kv", "turn_end."+s.ID, turnSignal{s, c.clock()})
		}
		if a.IdleAt != "" && a.IdleAt >= a.DeliveredAt {
			return nil
		}
		_, p, err := rd.policy()
		if err != nil {
			return err
		}
		e, err := engine.New(engine.Config{Store: lockedStore{repo, u}, Clock: c.clock, Policy: func() engine.Policy { return p }, Scope: ObjectKinds(nil)})
		if err != nil {
			return err
		}
		return finishTurn(ctx, e, w, "")
	})
}
