package cli

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"slices"
	"strconv"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
)

const pageSize = 50

func (i *invocation) view(fn func(app.CommandReader) (result, error)) (result, error) {
	var out result
	err := app.ViewCommands(i.ctx, i.home, func(r app.CommandReader) error {
		var err error
		out, err = fn(r)
		return err
	})
	return out, err
}

func (i *invocation) number(name string, fallback, low, high int64) (int64, error) {
	v, ok := i.flags[name]
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < low || n > high {
		return 0, invalid("%s 需要 %d 到 %d 的整数", name, low, high)
	}
	return n, nil
}

func status(i *invocation) (result, error) {
	return i.view(func(r app.CommandReader) (result, error) { s, err := r.Status(); return result{payload: s}, err })
}
func showPolicy(i *invocation) (result, error) {
	return i.view(func(r app.CommandReader) (result, error) { p, err := r.Policy(); return result{payload: p}, err })
}
func attention(i *invocation) (result, error) {
	return i.view(func(r app.CommandReader) (result, error) {
		items, err := r.Attention()
		if err != nil {
			return result{}, err
		}
		if c := i.flags["--cursor"]; c != "" {
			at := slices.IndexFunc(items, func(item engine.AttentionItem) bool { return item.ID == c })
			if at < 0 {
				return result{}, invalid("待办游标已失效")
			}
			items = items[at+1:]
		}
		page := map[string]any{"items": items[:min(len(items), pageSize)]}
		if len(items) > pageSize {
			page["nextCursor"] = items[pageSize-1].ID
		}
		return data(page)
	})
}

type workRow struct {
	ID        string       `json:"id"`
	Title     string       `json:"title"`
	Stage     engine.Stage `json:"stage"`
	UpdatedAt string       `json:"updatedAt"`
}

func workSummary(w engine.Work) workRow {
	return workRow{w.ID, clipText(w.Title), engine.WorkStage(w), w.UpdatedAt}
}

func listWorks(i *invocation) (result, error) {
	_, open := i.flags["--open"]
	thread := i.flags["--thread"]
	if thread != "" {
		if _, err := threadKeyRef(thread); err != nil {
			return result{}, err
		}
	}
	return i.view(func(r app.CommandReader) (result, error) {
		works, err := r.WorkPage(open, thread, i.flags["--cursor"], pageSize+1)
		if err != nil {
			return result{}, err
		}
		page := map[string]any{}
		if len(works) > pageSize {
			works = works[:pageSize]
			page["nextCursor"] = works[pageSize-1].ID
		}
		items := make([]workRow, len(works))
		for n, w := range works {
			items[n] = workSummary(w)
		}
		page["items"] = items
		return data(page)
	})
}

func events(i *invocation) (result, error) {
	since, err := i.number("--since", 0, 0, 1<<63-1)
	if err != nil {
		return result{}, err
	}
	return i.view(func(r app.CommandReader) (result, error) {
		ev, err := r.Events(since, pageSize+1)
		if err != nil {
			return result{}, err
		}
		page := map[string]any{}
		if len(ev) > pageSize {
			ev = ev[:pageSize]
			page["nextSince"] = ev[pageSize-1].Rev
		}
		items := make([]any, len(ev))
		for n, e := range ev {
			raw, err := json.Marshal(e.Data)
			if err != nil {
				return result{}, err
			}
			items[n] = map[string]any{"rev": e.Rev, "type": e.Type, "at": e.At, "ref": e.Ref, "data": preview(raw)}
		}
		page["items"] = items
		return data(page)
	})
}

func decisions(i *invocation) (result, error) {
	limit, err := i.number("--limit", pageSize, 1, 100)
	if err != nil {
		return result{}, err
	}
	kind := i.flags["--kind"]
	switch engine.DecisionKind(kind) {
	case "", engine.Ignore, engine.Accept, engine.Stop, engine.Reopen, engine.Unavailable:
	default:
		return result{}, invalid("未知决策类型 %q", kind)
	}
	_, observed := i.flags["--would-accept"]
	return i.view(func(r app.CommandReader) (result, error) {
		rows, err := r.Decisions(kind, i.flags["--source"], observed, i.flags["--cursor"], int(limit)+1)
		if err != nil {
			return result{}, err
		}
		page := map[string]any{}
		if len(rows) > int(limit) {
			rows = rows[:limit]
			page["nextCursor"] = rows[len(rows)-1].ID
		}
		items := make([]any, len(rows))
		out := result{payload: page}
		for n, d := range rows {
			item := map[string]any{"id": d.ID, "threadKey": d.ThreadKey, "at": d.At, "kind": d.Kind, "strength": d.Strength, "policyVersion": d.PolicyVersion}
			if d.WorkID != "" {
				item["workId"] = d.WorkID
			}
			if d.Dismissed {
				item["dismissed"] = true
			}
			if string(d.Audit["would_accept"]) == "true" {
				item["wouldAccept"] = true
			}
			if proposal := d.Audit["proposal"]; proposal != nil {
				item["proposal"] = preview(proposal)
			}
			items[n] = item
			ev, err := decisionEvidence(d)
			if err != nil {
				return result{}, err
			}
			out.evidence = append(out.evidence, ev)
		}
		page["items"] = items
		return out, nil
	})
}

func decisionEvidence(d engine.Decision) (map[string]any, error) {
	ev := map[string]any{"decisionId": d.ID}
	delete(d.Audit, "would_accept")
	delete(d.Audit, "proposal")
	if len(d.Audit) > 0 {
		raw, err := json.Marshal(d.Audit)
		if err != nil {
			return nil, err
		}
		ev["audit"] = preview(raw)
	}
	if len(d.Reason) > 0 {
		raw, err := json.Marshal(d.Reason)
		if err != nil {
			return nil, err
		}
		ev["reason"] = preview(raw)
	}
	if len(ev) == 1 {
		ev["missing"] = "研判记录缺少依据"
	}
	return ev, nil
}

func readWork(i *invocation) (result, error) {
	offset, err := i.number("--offset", 0, 0, int64(^uint(0)>>1))
	if err != nil {
		return result{}, err
	}
	return i.view(func(r app.CommandReader) (result, error) {
		w, triggers, total, err := r.WorkFacts(*i.binding, i.args[0], int(offset), pageSize)
		if err != nil {
			return result{}, err
		}
		return workFacts(w, triggers, total, int(offset))
	})
}

func workFacts(w *engine.Work, triggers []*engine.Trigger, total, offset int) (result, error) {
	latest := w.Assignments[len(w.Assignments)-1]
	view := map[string]any{"work": workSummary(*w), "decisionId": w.DecisionID, "assignmentId": latest.ID}
	if q := w.Attention; q != nil {
		view["attention"] = map[string]string{"id": q.ID, "question": clipText(q.Question), "answeredAt": q.AnsweredAt}
	}
	if latest.Report != nil {
		report := *latest.Report
		report.Summary = clipText(report.Summary)
		view["report"] = report
	}
	if w.Delivery != nil {
		view["delivery"] = map[string]string{"at": w.Delivery.At, "error": clipText(w.Delivery.Error)}
	}
	followStart := min(offset, len(w.Follows))
	followEnd := followStart + min(pageSize, len(w.Follows)-followStart)
	// 授权及工作位置属于当前工作。截短外部原文，不向其他对象扩展查询。
	for name, value := range map[string]any{"mandate": w.Mandate, "execution": w.Execution, "follows": w.Follows[followStart:followEnd]} {
		raw, err := jsonv2.Marshal(value, jsonv2.Deterministic(true))
		if err != nil {
			return result{}, fmt.Errorf("编码工作字段 %s: %w", name, err)
		}
		view[name] = preview(raw)
	}
	tr := make([]any, len(triggers))
	for n, t := range triggers {
		tr[n] = map[string]any{"id": t.ID, "source": t.Source, "event": t.Event, "at": t.At, "actor": t.Actor, "url": clipText(t.URL), "text": clipText(t.Text)}
	}
	view["triggers"] = tr
	start := min(offset, len(w.Inputs))
	end := start + min(pageSize, len(w.Inputs)-start)
	inputs := make([]any, 0, end-start)
	for _, in := range w.Inputs[start:end] {
		raw, err := jsonv2.Marshal(in)
		if err != nil {
			return result{}, fmt.Errorf("编码工作输入: %w", err)
		}
		inputs = append(inputs, preview(raw))
	}
	view["inputs"] = inputs
	if max(total, len(w.Inputs), len(w.Follows))-offset > pageSize {
		view["nextOffset"] = offset + pageSize
	}
	return data(view)
}
