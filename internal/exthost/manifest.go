package exthost

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Manifest 保存装载所需的声明；由其他部件解释的字段保留原始 JSON。
type Manifest struct {
	ID          string      `json:"id"`
	Version     string      `json:"version"`
	Protocol    int         `json:"protocol"`
	Exec        []string    `json:"exec"`
	Requires    []string    `json:"requires"`
	Alarms      []AlarmDecl `json:"alarms"`
	AlarmLimits struct {
		Concurrency *int `json:"concurrency"`
	} `json:"alarm_limits"`
	WatchSources   []WatchSource   `json:"watch_sources"`
	Actions        []ActionDecl    `json:"actions"`
	Ports          json.RawMessage `json:"ports"`
	PlacementRules json.RawMessage `json:"placement_rules"`
	Views          json.RawMessage `json:"views"`
}

// ActionDecl 用全局标识声明动作归属和确认要求。
type ActionDecl struct {
	ID         string `json:"id"`
	Level      string `json:"level"`
	Reversible bool   `json:"reversible"`
	ReadOnly   bool   `json:"read_only"`
}

// AlarmDecl 的时长已在装载时解析，应用层可直接登记。
type AlarmDecl struct {
	ID, At                 string
	Every, Jitter, Timeout time.Duration
	DefaultEnabled         bool
}

// WatchSource 描述扩展能报告的来源、对象种类、事件与事实键。
type WatchSource struct {
	ID          string       `json:"id"`
	ThreadKinds []ThreadKind `json:"thread_kinds"`
	Events      []EventDecl  `json:"events"`
	Facts       []string     `json:"facts"`
}

// ThreadKind 声明一个来源内的对象标识约定。
type ThreadKind struct {
	ID         string `json:"id"`
	Closable   bool   `json:"closable"`
	ObjectType string `json:"object_type"`
}

// EventDecl 供应用层构建事件目录。Source 取自外层来源，扩展不能在事件内覆盖。
type EventDecl struct {
	Source       string     `json:"-"`
	Event        string     `json:"id"`
	Label        string     `json:"label"`
	Default      string     `json:"default"`
	Strength     string     `json:"strength"`
	FollowupOnly bool       `json:"followup_only"`
	OnUnmatched  string     `json:"on_unmatched"`
	FeedTier     string     `json:"feed_tier"`
	Verb         string     `json:"verb"`
	NoTarget     string     `json:"no_target"`
	Rule         *EventRule `json:"rule"`
	Collapse     *Collapse  `json:"collapse"`
}

// EventRule 携带扩展为这类事件声明的规则说明。
type EventRule struct {
	Name        string `json:"name"`
	Instruction string `json:"instruction"`
}

// Collapse 声明同类事件的合并组以及可选的分组字段。
type Collapse struct {
	Group string `json:"group"`
	By    string `json:"by"`
}

// 已声明的字段有具体的 JSON 类型；省略可选字段和传入 null 含义不同。
// 未解释的 RawMessage 则必须连同 null 一起保留。
var manifestTypes = jsonv2.WithUnmarshalers(jsonv2.UnmarshalFromFunc(func(d *jsontext.Decoder, dest any) error {
	if _, opaque := dest.(*json.RawMessage); !opaque && d.PeekKind() == 'n' {
		return errors.New("字段不能为 null")
	}
	return errors.ErrUnsupported
}))

// UnmarshalJSON 区分省略的可选枚举和显式传入的非法空值。
func (e *EventDecl) UnmarshalJSON(b []byte) error {
	type plain EventDecl
	var v plain
	if err := jsonv2.Unmarshal(b, &v, manifestTypes); err != nil {
		return fmt.Errorf("watch_sources.events: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	for _, f := range []struct {
		key, value string
		optional   bool
		allowed    []string
	}{
		{"default", v.Default, false, []string{"off", "observe", "auto"}}, {"strength", v.Strength, false, []string{"explicit", "inferred"}},
		{"on_unmatched", v.OnUnmatched, true, []string{"filter", "record"}}, {"feed_tier", v.FeedTier, true, []string{"notice", "digest"}}, {"no_target", v.NoTarget, true, []string{"scratch"}},
	} {
		if f.optional && fields[f.key] == nil {
			continue
		}
		if !slices.Contains(f.allowed, f.value) {
			return fmt.Errorf("events[%q].%s 值 %q 无效，须为 %s", v.Event, f.key, f.value, strings.Join(f.allowed, "/"))
		}
	}
	switch {
	case v.Event == "":
		return fmt.Errorf("events.id 不能为空")
	case utf8.RuneCountInString(v.Verb) > 6:
		return fmt.Errorf("events[%q].verb 超过 6 个字符", v.Event)
	case fields["collapse"] != nil && (v.Collapse == nil || v.Collapse.Group == ""):
		return fmt.Errorf("events[%q].collapse.group 不能为空", v.Event)
	}
	*e = EventDecl(v)
	return nil
}

// UnmarshalJSON 把时间字符串转换成时长，并在字段省略时默认启用闹钟。
func (a *AlarmDecl) UnmarshalJSON(data []byte) error {
	var v struct {
		ID      string          `json:"id"`
		Every   json.RawMessage `json:"every"`
		At      json.RawMessage `json:"at"`
		Jitter  json.RawMessage `json:"jitter"`
		Timeout json.RawMessage `json:"timeout"`
		Enabled json.RawMessage `json:"default_enabled"`
	}
	if err := jsonv2.Unmarshal(data, &v, manifestTypes); err != nil {
		return fmt.Errorf("alarms: %w", err)
	}
	if v.ID == "" {
		return fmt.Errorf("alarms.id 不能为空")
	}
	if (v.Every == nil) == (v.At == nil) {
		return fmt.Errorf("alarms[%q] 必须声明 every、at 之一", v.ID)
	}
	out := AlarmDecl{ID: v.ID, DefaultEnabled: true}
	if v.Enabled != nil {
		if string(v.Enabled) != "true" && string(v.Enabled) != "false" {
			return fmt.Errorf("alarms[%q].default_enabled 必须为布尔值", v.ID)
		}
		out.DefaultEnabled = string(v.Enabled) == "true"
	}
	for _, f := range []struct {
		key  string
		raw  json.RawMessage
		dest *time.Duration
	}{{"every", v.Every, &out.Every}, {"jitter", v.Jitter, &out.Jitter}, {"timeout", v.Timeout, &out.Timeout}} {
		if f.raw == nil {
			continue
		}
		var s string
		if err := json.Unmarshal(f.raw, &s); err != nil {
			return fmt.Errorf("alarms[%q].%s: %w", v.ID, f.key, err)
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return fmt.Errorf("alarms[%q].%s: %w", v.ID, f.key, err)
		}
		if d < 0 || f.key == "every" && d == 0 {
			return fmt.Errorf("alarms[%q].%s 时长无效 %q", v.ID, f.key, s)
		}
		*f.dest = d
	}
	if v.At != nil {
		if err := json.Unmarshal(v.At, &out.At); err != nil {
			return fmt.Errorf("alarms[%q].at: %w", v.ID, err)
		}
		tm, err := time.Parse("15:04", out.At)
		if err != nil || tm.Format("15:04") != out.At {
			return fmt.Errorf("alarms[%q].at 须为 HH:MM", v.ID)
		}
	}
	*a = out
	return nil
}

func loadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err = jsonv2.Unmarshal(data, &m, manifestTypes); err == nil {
		err = m.validate()
	}
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ReservedSource 是内核内置人事件（craft:person.*）使用的 source。这些事件只经引擎命令产生，
// 不允许任何扩展声明或经 inbox 注入：它们的授权依据是「人说了算」，被伪造等于绕过授权。
const ReservedSource = "craft"

func (m *Manifest) validate() error {
	switch {
	case strings.Contains(m.ID, ":"):
		return fmt.Errorf("id %q 含冒号", m.ID)
	case !validID(m.ID):
		return fmt.Errorf("id 须为单个非空目录名")
	case m.Version == "":
		return fmt.Errorf("version 不能为空")
	case m.Protocol <= 0:
		return fmt.Errorf("protocol 须为正整数主版本")
	case len(m.Exec) == 0 || !filepath.IsLocal(m.Exec[0]) || filepath.Clean(m.Exec[0]) == ".":
		return fmt.Errorf("exec[0] 须为扩展目录内的相对文件名")
	}
	for _, id := range m.Requires {
		if !validID(id) {
			return fmt.Errorf("requires 含无效目录名 %q", id)
		}
	}
	// 省略并发限制由后续调度层处理，显式限制则至少要留一个槽。
	if n := m.AlarmLimits.Concurrency; n != nil && *n < 1 {
		return fmt.Errorf("alarm_limits.concurrency 须为正整数")
	}
	alarms := map[string]bool{}
	for _, a := range m.Alarms {
		if strings.Contains(a.ID, ":") {
			return fmt.Errorf("alarms.id %q 含冒号", a.ID)
		}
		if alarms[a.ID] {
			return fmt.Errorf("alarms.id 重复 %q", a.ID)
		}
		alarms[a.ID] = true
	}
	actions := make(map[string]bool)
	for _, a := range m.Actions {
		if a.ID == "" || actions[a.ID] {
			return fmt.Errorf("actions.id 为空或重复 %q", a.ID)
		}
		switch a.Level {
		case "auto", "confirm", "confirm_named", "deny":
		default:
			return fmt.Errorf("actions[%q].level 无效 %q", a.ID, a.Level)
		}
		actions[a.ID] = true
	}
	events := make(map[string]bool)
	for i := range m.WatchSources {
		s := &m.WatchSources[i]
		if strings.Contains(s.ID, ":") {
			return fmt.Errorf("watch_sources[%d].id %q 含冒号", i, s.ID)
		}
		if s.ID == "" {
			return fmt.Errorf("watch_sources[%d].id 不能为空", i)
		}
		if s.ID == ReservedSource {
			return fmt.Errorf("watch_sources[%d].id 不能使用保留名 %q：它属于内核内置的人事件，扩展声明它就能伪造「人发起的请求」", i, s.ID)
		}
		for j, k := range s.ThreadKinds {
			if strings.Contains(k.ID, ":") {
				return fmt.Errorf("watch_sources[%d].thread_kinds[%d].id %q 含冒号", i, j, k.ID)
			}
			if k.ID == "" {
				return fmt.Errorf("watch_sources[%d].thread_kinds[%d].id 不能为空", i, j)
			}
		}
		for j := range s.Events {
			e := &s.Events[j]
			key := s.ID + ":" + e.Event
			if events[key] {
				return fmt.Errorf("watch_sources.events 重复 %q", key)
			}
			events[key] = true
			e.Source = s.ID
		}
	}
	return nil
}

func validID(s string) bool { return filepath.IsLocal(s) && s != "." && s == filepath.Base(s) }
