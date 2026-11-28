package app

import (
	"maps"
	"sync"
)

// HealthEntry 保存最近一次状态及原因。扩展上报的条目按扩展 ID 单独保存。
type HealthEntry struct {
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// DaemonHealth 是独立的只读快照；被拒绝的 manifest 按路径区分重名扩展。
type DaemonHealth struct {
	Extensions map[string]HealthEntry
	Rejected   map[string]HealthEntry
	Reports    map[string]map[string]HealthEntry
	Alarms     map[string]HealthEntry
	Effects    map[string]HealthState
}

type daemonHealth struct {
	mu      sync.Mutex
	state   DaemonHealth
	changed func()
}

func newDaemonHealth(changed func()) *daemonHealth {
	return &daemonHealth{changed: changed, state: DaemonHealth{
		Extensions: map[string]HealthEntry{},
		Rejected:   map[string]HealthEntry{},
		Reports:    map[string]map[string]HealthEntry{},
		Alarms:     map[string]HealthEntry{},
		Effects:    map[string]HealthState{},
	}}
}

func (h *daemonHealth) snapshot() DaemonHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.state
	s.Extensions = maps.Clone(s.Extensions)
	s.Rejected = maps.Clone(s.Rejected)
	s.Alarms = maps.Clone(s.Alarms)
	s.Effects = maps.Clone(s.Effects)
	s.Reports = maps.Clone(s.Reports)
	for id, m := range s.Reports {
		s.Reports[id] = maps.Clone(m)
	}
	return s
}

func (h *daemonHealth) notify(changed bool) {
	if changed && h.changed != nil {
		h.changed()
	}
}

func (h *daemonHealth) set(entries map[string]HealthEntry, key, status, detail string) {
	h.mu.Lock()
	entry := HealthEntry{status, detail}
	previous := entries[key]
	entries[key] = entry
	h.mu.Unlock()
	h.notify(entry != previous)
}

func (h *daemonHealth) report(id, key string, entry HealthEntry) {
	h.mu.Lock()
	m := h.state.Reports[id]
	if m == nil {
		m = map[string]HealthEntry{}
		h.state.Reports[id] = m
	}
	old := m[key]
	m[key] = entry
	h.mu.Unlock()
	h.notify(entry != old)
}

func (h *daemonHealth) effect(kind string, state HealthState) {
	h.mu.Lock()
	old := h.state.Effects[kind]
	h.state.Effects[kind] = state
	h.mu.Unlock()
	h.notify(state != old)
}
