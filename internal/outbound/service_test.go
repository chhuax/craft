package outbound_test

import (
	"testing"

	"github.com/chhuax/craft/internal/outbound"
	"github.com/chhuax/craft/internal/outbound/outboundtest"
)

// TestMemory 用内存仓储运行完整的外发契约套件。
func TestMemory(t *testing.T) {
	outboundtest.Run(t, func(*testing.T) outbound.Outbox { return outbound.NewMemory() })
}
