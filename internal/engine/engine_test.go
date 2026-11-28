package engine_test

import (
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/engine/enginetest"
)

func TestEngine(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	enginetest.Run(t, func(*testing.T) engine.Store { return engine.NewMemory() })
}
