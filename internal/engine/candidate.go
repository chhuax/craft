package engine

import (
	"context"
	"encoding/json"
	"fmt"
)

// Candidate 是一次研判的一致读快照，调用方可独占修改。Works 含该对象订阅的已结束工作。
type Candidate struct {
	EvalID        string        `json:"evalId"`
	Thread        Thread        `json:"thread"`
	Triggers      []Trigger     `json:"triggers"`
	Strength      Strength      `json:"strength"`
	PolicyVersion string        `json:"policyVersion"`
	Works         []Work        `json:"works"`
	Related       []RelatedWork `json:"related"`
}

// RelatedWork 只投影其他在办工作的归属和事实，供研判方选择挂接目标。
type RelatedWork struct {
	ID      string                     `json:"id"`
	Title   string                     `json:"title"`
	Stage   Stage                      `json:"stage"`
	Threads []string                   `json:"threads"`
	Facts   map[string]map[string]Fact `json:"facts"`
}

// Assessor 由运行层驱动；Request 接收候选快照，Release 按研判代次释放资源。
type Assessor interface {
	Request(Candidate) error
	Release(evalID string) error
}

// Candidate 按活跃研判标识生成有界卡片。已结束的代次返回 evalFinished。
func (e *Engine) Candidate(ctx context.Context, evalID string) (Candidate, error) {
	var c Candidate
	err := e.cfg.Store.Read(ctx, func(r Reader) error {
		th, err := r.ThreadByEval(evalID)
		if err != nil {
			return err
		}
		if th == nil {
			return evalFinished(evalID)
		}
		ev := th.Evaluating
		triggers, err := r.GetTriggers(ev.TriggerIDs...)
		if err != nil {
			return err
		}
		c = Candidate{EvalID: evalID, Thread: *th, Strength: ev.Strength, PolicyVersion: ev.PolicyVersion}
		for _, id := range ev.TriggerIDs {
			tr := triggers[id]
			if tr == nil {
				return fmt.Errorf("批内 Trigger %q 缺失", id)
			}
			if text := clipChars(tr.Text, 4000); text != tr.Text {
				tr.Text = text
				if tr.Meta == nil {
					tr.Meta = make(map[string]json.RawMessage)
				}
				tr.Meta["truncated"] = json.RawMessage("true")
			}
			c.Triggers = append(c.Triggers, *tr)
		}
		rows, err := r.GetWorks(th.Subscriptions...)
		if err != nil {
			return err
		}
		c.Works = worksInOrder(th.Subscriptions, rows)
		for i := range c.Works {
			boundWorkCard(&c.Works[i])
		}
		c.Related, err = relatedWorks(r, th.Subscriptions)
		return err
	})
	if err != nil {
		return Candidate{}, fmt.Errorf("读取研判 %q 卡片: %w", evalID, err)
	}
	return c, nil
}

func relatedWorks(r Reader, exclude []string) ([]RelatedWork, error) {
	works, err := r.RecentOpenWorks(8, exclude...)
	if err != nil {
		return nil, err
	}
	related := make([]RelatedWork, 0, len(works))
	var keys []string
	for i := range works {
		w := &works[i]
		card := RelatedWork{ID: w.ID, Title: w.Title, Stage: WorkStage(*w), Facts: make(map[string]map[string]Fact)}
		for _, f := range w.Follows {
			if f.Own {
				card.Threads = append(card.Threads, f.Thread)
			}
		}
		keys = append(keys, card.Threads...)
		related = append(related, card)
	}
	threads, err := r.GetThreads(keys...)
	if err != nil {
		return nil, err
	}
	for i := range related {
		for _, key := range related[i].Threads {
			if th := threads[key]; th != nil && len(th.Meta.Facts) > 0 {
				related[i].Facts[key] = th.Meta.Facts
			}
		}
	}
	return related, nil
}

func boundWorkCard(w *Work) {
	for i := range w.Inputs {
		in := &w.Inputs[i]
		if in.Text != nil {
			*in.Text = clipChars(*in.Text, 400)
		}
		in.Note = clipChars(in.Note, 400)
	}
	if w.Attention != nil {
		w.Attention.Question = clipChars(w.Attention.Question, 400)
	}
	for i := range w.Assignments {
		if r := w.Assignments[i].Report; r != nil {
			r.Summary = clipChars(r.Summary, 600)
		}
	}
	if w.Execution != nil {
		w.Execution.Assignment = clipChars(w.Execution.Assignment, 1500)
	}
}
