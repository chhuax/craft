package engine_test

import (
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/engine/repotest"
)

func TestMemory(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	repotest.Run(t, func(*testing.T) engine.Store { return engine.NewMemory() })
}
