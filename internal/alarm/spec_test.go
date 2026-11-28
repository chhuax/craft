package alarm

import (
	"context"
	"testing"
	"time"
)

func TestAlarmSpec(t *testing.T) {
	for _, tc := range []struct {
		at    string
		every time.Duration
		valid bool
	}{
		{"", 0, false}, {"12:30", time.Hour, false}, {"", -1, false},
		{"7:30", 0, false}, {"24:00", 0, false}, {"09:60", 0, false},
		{"23:59", 0, true}, {"00:00", 0, true}, {"", time.Nanosecond, true},
	} {
		spec := AlarmSpec{ID: "a", Every: tc.every, At: tc.at, Run: func(context.Context, RunRequest) error { return nil }}
		s, err := NewService(Config{GlobalLimit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Register(spec); (err == nil) != tc.valid {
			t.Errorf("Every=%v At=%q: %v", tc.every, tc.at, err)
		}
	}
}
