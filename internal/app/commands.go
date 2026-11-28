package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
)

// SessionBinding 只有原生会话身份时须先查询归属。完整绑定仍须在命令事务内核对。
type SessionBinding struct {
	WorkID     string
	Session    engine.Session
	Assignment string
}

// StateHome 解析命令使用的状态目录，仅读取状态时无需创建目录。
func StateHome(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	dir := getenv("CRAFT_HOME")
	if dir == "" {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("查找用户目录: %w", err)
		}
		dir = filepath.Join(home, ".craft")
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("解析状态目录: %w", err)
	}
	return absolute, nil
}

// CommandReader 的索引与聚合来自同一快照，只能在 ViewCommands 的回调内使用。
type CommandReader struct{ read *store.SQLiteReader }

// ViewCommands 将尚未创建的库呈现为空状态。已有库始终以只读方式打开。
func ViewCommands(ctx context.Context, home string, fn func(CommandReader) error) error {
	path := filepath.Join(home, "craft.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fn(CommandReader{})
	} else if err != nil {
		return fmt.Errorf("检查状态库: %w", err)
	}
	return store.ViewFile(ctx, path, func(r store.SQLiteReader) error { return fn(CommandReader{&r}) })
}

func (r CommandReader) lookup(name string, args ...any) ([]store.Doc, error) {
	if r.read == nil {
		return nil, nil
	}
	return r.read.Lookup(name, args...)
}

func (r CommandReader) sessionWork(s engine.Session) (*engine.Work, error) {
	if r.read == nil || s.Host == "" || s.ID == "" {
		return nil, nil
	}
	works, err := (engineReader{*r.read}).works("sessionWorks", s.Host, s.ID)
	if err != nil || len(works) == 0 {
		return nil, err
	}
	// 索引把 open Work 排在最前；若第二项也 open，就无法唯一认定归属。
	if len(works) > 1 && works[1].Ended == nil {
		return nil, &engine.UserError{Code: engine.CodeNoWorkForSession}
	}
	return &works[0], nil
}

func (r CommandReader) boundWork(b SessionBinding) (*engine.Work, error) {
	w, err := r.sessionWork(b.Session)
	if err != nil {
		return nil, err
	}
	if w == nil || b.WorkID != "" && w.ID != b.WorkID {
		return nil, &engine.UserError{Code: engine.CodeNoWorkForSession}
	}
	if w.Ended != nil {
		return nil, &engine.UserError{Code: engine.CodeWorkEnded}
	}
	if len(w.Assignments) == 0 || b.Assignment == "" {
		return nil, &engine.UserError{Code: engine.CodeUnknownAssignment}
	}
	a := w.Assignments[len(w.Assignments)-1]
	if a.ID != b.Assignment || (a.State != engine.AssignmentPending && a.State != engine.AssignmentRunning) {
		return nil, &engine.UserError{Code: engine.CodeAssignmentEnded}
	}
	if w.Execution.Assignment != "" && w.Execution.Assignment != b.Assignment {
		return nil, &engine.UserError{Code: engine.CodeNoWorkForSession}
	}
	return w, nil
}

type lockedStore struct {
	*engineStore
	unit *engineUnit
}

func (s lockedStore) Read(_ context.Context, fn func(engine.Reader) error) error { return fn(s.unit) }

func (s lockedStore) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	return s.unit.Atomic(ctx, fn)
}

// Commands 管理短进程的写连接，调用方须 Close。它只提交命令，不启动 Pass 或消费 effect。
type Commands struct {
	db    *store.SQLite
	clock engine.Clock
	scope engine.SourceScope
}

// OpenCommands 打开或初始化状态库。scope 是已加载的本地声明，clock 由进程入口注入。
func OpenCommands(ctx context.Context, home string, clock engine.Clock, scope engine.SourceScope) (*Commands, error) {
	if clock == nil {
		return nil, errors.New("命令需要 UTC 毫秒时钟")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("创建状态目录: %w", err)
	}
	db, err := store.OpenSQLite(ctx, filepath.Join(home, "craft.db"), nil)
	if err != nil {
		return nil, err
	}
	return &Commands{db, clock, scope}, nil
}

// Close 在命令结束后释放读写连接。
func (c *Commands) Close() error { return c.db.Close() }

// Execute 在写锁内读取策略、核对会话并运行一条引擎命令。检查和写回之间不能切换派工。
func (c *Commands) Execute(ctx context.Context, b *SessionBinding, fn func(*engine.Engine, string) error) error {
	repo := &engineStore{db: c.db}
	return c.db.Write(ctx, func(tx *store.SQLiteTx) error {
		reader := CommandReader{&tx.SQLiteReader}
		var id string
		if b != nil {
			w, err := reader.boundWork(*b)
			if err != nil {
				return err
			}
			id = w.ID
		}
		prev, policy, err := reader.policy()
		if err != nil {
			return err
		}
		// 首条命令也要留下实际使用的策略，以便首次改版有完整的旧授权依据。
		if prev.Version == "" {
			prev.Version = policy.Version()
			if _, err := savePolicy(tx, prev); err != nil {
				return err
			}
		}
		scope := c.scope
		var missing missingObjectKinds
		if scope == nil {
			// 空库仅有内置事件。有已生效的外部目录时，是否可关闭不能从事件声明推导。
			scope = ObjectKinds(nil)
			if len(prev.Catalog) > 0 {
				scope = &missing
			}
		}
		e, err := engine.New(engine.Config{Store: lockedStore{repo, repo.in(ctx, tx)}, Clock: c.clock, Scope: scope, Policy: func() engine.Policy { return policy }})
		if err != nil {
			return err
		}
		err = fn(e, id)
		if missing.consulted {
			return ErrObjectKinds
		}
		return err
	})
}

// ObjectKinds 是从 manifest 得到的可关闭对象集合。短命令不接收扩展事件。
type ObjectKinds map[[2]string]bool

// Closable 核对来源与对象类型的组合。
func (k ObjectKinds) Closable(source, kind string) bool { return k[[2]string{source, kind}] }

// Scope 将外部事件接收入口保留给运行层。
func (ObjectKinds) Scope(string, string, map[string]json.RawMessage) engine.ScopeResult {
	return engine.ScopeResult{}
}

// ErrObjectKinds 表示本次命令需要扩展的对象声明，只有事件目录不足以核对归属。
var ErrObjectKinds = errors.New("缺少已加载的扩展对象声明，无法核对可关闭对象的归属")

type missingObjectKinds struct {
	ObjectKinds
	consulted bool
}

// SourceScope 的接口不能返回错误。仅在引擎确实用到该能力时撤销整条命令，
// 使重复的人工请求和在此之前已经确定的业务拒绝仍保留原语义。
func (s *missingObjectKinds) Closable(string, string) bool { s.consulted = true; return false }

// LoadObjectKinds 从安装方提供的根目录读取 manifest，一次命令只扫描一次，不启动扩展进程。
func LoadObjectKinds(dirs []string) (ObjectKinds, error) {
	discovery := exthost.Discover(dirs...)
	if len(discovery.Rejected) > 0 {
		path := slices.Min(slices.Collect(maps.Keys(discovery.Rejected)))
		return nil, fmt.Errorf("读取扩展声明 %q: %w", path, discovery.Rejected[path])
	}
	kinds := ObjectKinds{}
	for _, ext := range discovery.Extensions {
		for _, source := range ext.Manifest.WatchSources {
			for _, kind := range source.ThreadKinds {
				kinds[[2]string{source.ID, kind.ID}] = kind.Closable
			}
		}
	}
	return kinds, nil
}
