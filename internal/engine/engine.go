package engine

// Config 提供命令依赖的仓储、来源规则与 UTC 毫秒时钟，由调用方注入。
type Config struct {
	Store Store
	Scope SourceScope
	Clock Clock
}

// Engine 在各自的写事务内执行命令；实例只持有依赖，可以跨命令复用。
type Engine struct{ cfg Config }

// New 组装引擎。Store 和 Clock 供所有命令使用，Receive 还需要 Scope。
func New(cfg Config) *Engine { return &Engine{cfg: cfg} }
