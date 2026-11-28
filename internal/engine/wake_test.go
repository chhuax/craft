package engine

import (
	"encoding/json"
	"testing"
)

func TestWake(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	for _, tc := range []struct {
		name string
		ids  []string
		want int
	}{
		{"无待投输入", nil, 0},
		{"多条输入合成一次投递请求", []string{"first", "second"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewMemory().(*memoryStore)
			w := Work{ID: "a", Undelivered: tc.ids}
			if err := s.Atomic(t.Context(), func(u Unit) error { return wake(u, &w) }); err != nil {
				t.Fatal(err)
			}
			effects := s.committed.Load().effects
			if len(effects) != tc.want {
				t.Fatalf("入队次数 %d，期望 %d", len(effects), tc.want)
			}
			for _, effect := range effects {
				var payload struct {
					WorkID string `json:"workId"`
				}
				if err := json.Unmarshal(effect.Value, &payload); err != nil {
					t.Fatal(err)
				}
				if effect.Kind != "delivery" || effect.Key != w.ID || payload.WorkID != w.ID {
					t.Fatalf("投递请求 %+v", effect)
				}
			}
		})
	}
}
