// Package enginetest 固定引擎命令的可观察行为，供各仓储适配器共用。
package enginetest

import (
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

const now = "2026-01-02T00:00:00.000Z"

// Run 为每组命令提供独立空仓储；持久化适配器应在 factory 中使用 t.TempDir 建库。
func Run(t *testing.T, factory func(t *testing.T) engine.Store) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		check func(*testing.T, engine.Store)
	}{
		{"对象状态事实", threadStatus},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, factory(t)) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
