package app

import (
	"context"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

// SessionFromEnv 解析进程入口的派工声明，实际归属由 boundWork 在命令事务内核对。
// 会话尚无规范 ID 时保留空绑定，以免启动窗口中的执行方落入人的命令面。
func SessionFromEnv(getenv func(string) string) *SessionBinding {
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
