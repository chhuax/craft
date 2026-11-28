package engine_test

import (
	"errors"
	"testing"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/engine/enginetest"
)

func TestEngine(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	enginetest.Run(t, func(*testing.T) engine.Store { return engine.NewMemory() })
}

func TestNewRequiresPolicy(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	e, err := engine.New(engine.Config{Store: engine.NewMemory(), Clock: func() string { return "2026-01-02T00:00:00.000Z" }})
	var rejection *engine.UserError
	if e != nil || err == nil || errors.As(err, &rejection) {
		t.Fatalf("缺少策略提供者应返回编程错误，得到 %v", err)
	}
}
