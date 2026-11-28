package conformance

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

type call struct {
	event
	arg   any
	err   error
	reply *event
}

func (s *suite) calls(events []event) []*call {
	var calls []*call
	pending := map[string]*call{}
	for _, e := range events {
		switch {
		case e.in && e.Method != "":
			arg, err := s.scope.Decode(e.Method, e.Params)
			c := &call{event: e, arg: arg, err: err}
			calls = append(calls, c)
			if e.ID != nil {
				pending[string(e.ID)] = c
			}
		case !e.in && e.Method == "" && e.ID != nil:
			if c := pending[string(e.ID)]; c != nil {
				c.reply = &e
				delete(pending, string(e.ID))
			}
		}
	}
	return calls
}

func (s *suite) inspect(events []event) {
	all := s.calls(events)
	byStage := map[string][]*call{}
	for _, c := range all {
		byStage[c.stage] = append(byStage[c.stage], c)
	}
	rounds := s.rounds
	ids := make([]map[string]bool, len(rounds))
	seenCursor, uncertainCursor := false, false
	for i, runs := range rounds {
		objects := map[string]bool{}
		var roundCalls []*call
		ids[i] = map[string]bool{}
		for _, run := range runs {
			calls := byStage[run.label]
			delete(byStage, run.label)
			for _, c := range calls {
				s.checkCall(c, objects, ids[i])
			}
			roundCalls = append(roundCalls, calls...)
			seen, uncertain := s.checkCursor(run, calls)
			seenCursor = seenCursor || seen
			uncertainCursor = uncertainCursor || uncertain
			s.checkMapping(run, calls)
		}
		s.checkOrder(roundCalls)
		if len(runs) == len(s.ext.Manifest.Alarms) {
			s.compareObjects(i+1, objects)
		}
	}
	// 握手、取消和关闭期间的对向调用也在权限及 JSON 约定之内，但不计入三次重放的集合。
	otherObjects, otherIDs := map[string]bool{}, map[string]bool{}
	for _, c := range all {
		if byStage[c.stage] != nil {
			s.checkCall(c, otherObjects, otherIDs)
		}
	}
	s.compareIDs(ids)
	if uncertainCursor {
		s.r.skip(cursor, "state/set 缺少游标身份或对应分页组，无法判定这次写入的顺序")
	} else if seenCursor {
		s.r.pass(cursor)
	} else {
		s.r.skip(cursor, "未观察到游标提交与对应的 watch/receive")
	}
}

func (s *suite) checkCall(c *call, objects, ids map[string]bool) {
	switch c.Method {
	case "watch/receive":
		p, ok := c.arg.(*protocol.Trigger)
		if ok {
			if p.ID != "" {
				ids[p.ID] = true
			}
			cross := false
			for _, ref := range p.Threads {
				if !s.ownsSource(ref.Source) {
					s.r.fail(sources, "%s: threads 中的来源 %q 应在 watch_sources 中声明", c.where(), ref.Source)
					cross = true
				} else {
					s.r.pass(sources)
				}
				s.observeObject(c, ref, objects)
			}
			if cross {
				return
			}
		}
	case "thread/describe":
		// 即使事实形状导致真内核拒绝，也要把原始调用归到 §5.5。
		if err := s.checkFacts(c); err != nil {
			s.r.fail(facts, "%s: %v", c.where(), err)
			return
		}
		if p, ok := c.arg.(*exthost.Describe); ok && p.Ref != nil {
			s.observeObject(c, *p.Ref, objects)
		}
	}
	if c.err != nil {
		s.r.fail(alarms, "%s: 期望已声明且参数有效的调用: %v", c.where(), c.err)
	}
}

func (s *suite) ownsSource(id string) bool {
	return slices.ContainsFunc(s.ext.Manifest.WatchSources, func(x exthost.WatchSource) bool { return x.ID == id })
}

func (s *suite) observeObject(c *call, ref protocol.ThreadRef, objects map[string]bool) {
	k, ok := s.scope.Kind(ref.Source, ref.Kind)
	if !ok || k.ObjectType == "" {
		return
	}
	id := ref.Kind + ":" + ref.ID
	objects[id] = true
	if want := s.replay.f.expect.ThreadIDs; want != nil && !slices.Contains(want, id) {
		s.r.fail(identity, "%s: 对象 %q 不在 expect.json 的 threadIds 内", c.where(), id)
	}
}

func (s *suite) compareObjects(round int, objects map[string]bool) {
	want := s.replay.f.expect.ThreadIDs
	if want == nil {
		s.r.skip(identity, "expect.json 缺少 threadIds，无法核对对象标识")
		return
	}
	for _, id := range want {
		if !objects[id] {
			s.r.fail(identity, "第 %d 轮 alarm/run 完成后，缺少预期对象 %q", round, id)
			return
		}
	}
	s.r.pass(identity)
}

func (s *suite) compareIDs(ids []map[string]bool) {
	if len(ids) != 3 {
		return
	}
	for i, set := range ids[1:] {
		if !maps.Equal(ids[0], set) {
			for _, id := range slices.Sorted(maps.Keys(ids[0])) {
				if !set[id] {
					s.r.fail(determinism, "第 %d 轮 alarm/run 重放，期望再次 receive %q", i+2, id)
					return
				}
			}
			for _, id := range slices.Sorted(maps.Keys(set)) {
				if !ids[0][id] {
					s.r.fail(determinism, "第 %d 轮 alarm/run 多出 receive %q，期望与首轮集合一致", i+2, id)
					return
				}
			}
		}
	}
	if len(ids[0]) > 0 {
		s.r.pass(determinism)
	}
}

func (s *suite) checkCursor(run trial, calls []*call) (seen, uncertain bool) {
	// 同一 Trigger 的重试只要最终拿到了成功回执即可。请求被排队或被拒绝都不算落盘。
	pending := map[string]bool{}
	last := 0
	for _, c := range calls {
		p, ok := c.arg.(*protocol.Trigger)
		if !ok {
			continue
		}
		pending[p.ID] = true
		if c.reply != nil && c.reply.Error == nil {
			delete(pending, p.ID)
			last = max(last, c.reply.number)
		}
	}
	if last == 0 && len(pending) == 0 {
		return false, false
	}
	pages := 0
	for _, e := range run.events {
		if e.httpStatus >= 200 && e.httpStatus < 300 {
			pages++
		}
	}
	for _, c := range calls {
		p, ok := c.arg.(*exthost.SetState)
		if !ok || p.Key == "blocked_until" {
			continue
		}
		if c.number >= last && len(pending) == 0 {
			// 所有候选状态写都晚于所有回执时，键名和分组不会影响结论。
			seen = true
			continue
		}
		if p.Key == "cursor" && pages <= 1 {
			s.r.fail(cursor, "%s: 游标必须在对应 watch/receive 成功返回之后提交", c.where())
		} else {
			// 私有键也可以保存差分索引；多个分页组可分别提交，不能按键名前缀或闹钟名硬判失败。
			uncertain = true
		}
	}
	return seen, uncertain
}

func (s *suite) checkWindow(events []event) {
	param := s.replay.f.expect.Lookback.Param
	if param == "" {
		order := "游标顺序已核对"
		if s.r[cursor].Status == "skip" {
			order = s.r[cursor].Detail
		}
		s.r.skip(cursor, "expect.json 缺少 lookback.param，跳过回看窗口核对；"+order)
		return
	}
	seen := false
	limit := s.now.Add(-7 * 24 * time.Hour)
	for _, e := range events {
		for _, v := range e.query[param] {
			seen = true
			t, err := parseInstant(v)
			if err != nil || t.Before(limit) {
				s.r.fail(cursor, "%s: %s=%q，期望回看起点不早于 %s", e.where(), param, v, limit.Format(time.RFC3339Nano))
			}
		}
	}
	if !seen {
		s.r.skip(cursor, "HTTP 请求未携带声明的 lookback.param，无法核对窗口")
	}
}

func parseInstant(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, v); err == nil {
		return t, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err == nil && len(v) == 13 {
		return time.UnixMilli(n), nil
	}
	return time.Unix(n, 0), err
}

func (s *suite) checkOrder(calls []*call) {
	// 最新状态与一般事件不能只凭事件名区分；只对本轮实际核实状态的对象配对。
	changes, descriptions := map[protocol.ThreadRef]bool{}, map[protocol.ThreadRef]bool{}
	for _, c := range calls {
		switch p := c.arg.(type) {
		case *exthost.Describe:
			if p.Ref != nil {
				descriptions[*p.Ref] = true
			}
		case *exthost.Closed:
			changes[p.Thread] = true
		case *exthost.Reopened:
			changes[p.Thread] = true
		}
	}
	described, changed := map[protocol.ThreadRef]bool{}, map[protocol.ThreadRef]bool{}
	for _, c := range calls {
		switch p := c.arg.(type) {
		case *exthost.Describe:
			if p.Ref != nil {
				described[*p.Ref] = true
			}
		case *exthost.Closed:
			changed[p.Thread] = true
		case *exthost.Reopened:
			changed[p.Thread] = true
		case *protocol.Trigger:
			for _, ref := range p.Threads {
				if descriptions[ref] && !described[ref] {
					s.r.fail(factOrder, "%s: %s 的 describe 应先于 watch/receive", c.where(), key(ref))
				}
				if !changes[ref] {
					continue
				}
				if !described[ref] || !changed[ref] {
					s.r.fail(factOrder, "%s: %s 应先 describe 并 closed 或 reopened", c.where(), key(ref))
				} else {
					s.r.pass(factOrder)
				}
			}
		}
	}
}

func (s *suite) checkFacts(c *call) error {
	var p struct {
		Facts map[string]json.RawMessage `json:"facts"`
	}
	if err := jsonv2.Unmarshal(c.Params, &p); err != nil {
		return fmt.Errorf("facts 应为嵌套对象: %w", err)
	}
	for _, id := range slices.Sorted(maps.Keys(p.Facts)) {
		raw := p.Facts[id]
		if bytes.Equal(raw, []byte("null")) {
			continue
		}
		var f exthost.Fact
		if err := jsonv2.Unmarshal(raw, &f); err != nil || f.Value == nil {
			return fmt.Errorf("facts[%q] 需要 {value,label?,problem?}", id)
		}
		if err := factValue(id, f.Value); err != nil {
			return err
		}
		s.r.pass(facts)
	}
	return nil
}

func factValue(id string, value json.RawMessage) error {
	switch id {
	case "title", "state", "due", "urgency":
	default:
		return nil
	}
	var v string
	if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, &v) != nil {
		return fmt.Errorf("facts[%q].value 期望字符串", id)
	}
	switch id {
	case "due":
		if _, err := time.Parse(time.DateOnly, v); err != nil {
			if _, err := time.Parse(time.RFC3339Nano, v); err != nil {
				return fmt.Errorf("due=%q，期望 ISO 日期或时间", v)
			}
		}
	case "urgency":
		if !slices.Contains([]string{"low", "normal", "high", "urgent"}, v) {
			return fmt.Errorf("urgency=%q，期望 low/normal/high/urgent", v)
		}
	}
	return nil
}

func (s *suite) checkMapping(run trial, calls []*call) {
	errorScenario := false
	for i, e := range run.events {
		if e.httpStatus == 0 || e.detail != "" {
			continue
		}
		next := len(run.events) - 1
		for j := i + 1; j < len(run.events); j++ {
			if run.events[j].httpStatus != 0 {
				next = j - 1
				break
			}
		}
		var reports []string
		blocked := false
		for _, c := range calls {
			if c.number <= e.number || c.number > run.events[next].number {
				continue
			}
			switch p := c.arg.(type) {
			case *exthost.HealthReport:
				if c.err == nil {
					reports = append(reports, p.State)
				}
			case *exthost.SetState:
				if p.Key == "blocked_until" && c.err == nil {
					var v string
					if len(p.Value) > 0 && p.Value[0] == '"' {
						_ = json.Unmarshal(p.Value, &v)
					} else {
						v = string(p.Value)
					}
					t, err := parseInstant(v)
					blocked = err == nil && t.After(s.now)
				}
			case *exthost.Closed:
				if e.empty || e.httpStatus == 401 || e.httpStatus == 429 || e.httpStatus >= 500 {
					s.r.fail(mapping, "%s: %s 的 HTTP %d 未确认对象不存在，应保留对象", c.where(), e.request, e.httpStatus)
				}
			}
		}
		switch {
		case e.httpStatus == 401:
			errorScenario = true
			if !slices.Contains(reports, "paused") || run.err != nil {
				s.r.fail(mapping, "%s: 401 后期望 health/report paused 并正常返回", e.where())
			} else {
				s.r.pass(mapping)
			}
		case e.httpStatus == 429:
			errorScenario = true
			if !blocked || run.err != nil {
				s.r.fail(mapping, "%s: 429 后期望写入 blocked_until 并正常返回", e.where())
			} else {
				s.r.pass(mapping)
			}
		case e.httpStatus >= 500:
			errorScenario = true
			_, mappedError := errors.AsType[*protocol.Error](run.err)
			if !slices.Contains(reports, "error") || !mappedError {
				s.r.fail(mapping, "%s: HTTP %d 后期望 health/report error 并以 RPC 错误返回", e.where(), e.httpStatus)
			} else {
				s.r.pass(mapping)
			}
		}
	}
	if run.err != nil && !errorScenario {
		s.r.fail(alarms, "%s: 期望正常返回: %v", run.label, run.err)
	}
}
