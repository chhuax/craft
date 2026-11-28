package engine

import "errors"

// Config 提供命令使用的仓储、来源规则与 UTC 毫秒时钟。Policy 在写事务开始时
// 提供当前策略快照；提供者不得做 IO，同一条命令始终使用同一版策略。
type Config struct {
	Store  Store
	Scope  SourceScope
	Clock  Clock
	Policy func() Policy
}

// Engine 在各自的写事务内执行命令；实例只持有依赖，可以跨命令复用。
type Engine struct{ cfg Config }

// New 组装引擎。Store、Clock、Policy 必填，使用 Receive 时还须提供 Scope。
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.Clock == nil || cfg.Policy == nil {
		return nil, errors.New("引擎需要 Store、Clock 与策略快照提供者")
	}
	return &Engine{cfg: cfg}, nil
}
