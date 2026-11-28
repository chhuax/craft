package engine

import (
	"context"
	"encoding/json"
	"fmt"
)

// Answer 将人的回答直投给当前问题所在的 Work。同一毫秒同一问题的相同回答可安全重放。
func (e *Engine) Answer(ctx context.Context, workID, text string) (Work, error) {
	return e.withWork(ctx, "回答问题", workID, false, func(u Unit, w *Work, now string) error {
		if w.Ended != nil {
			if w.Ended.By == "human" {
				return &UserError{Code: CodeWorkStopped}
			}
			return &UserError{Code: CodeWorkEnded}
		}
		q := w.Attention
		if q == nil {
			return &UserError{Code: CodeNoAttention}
		}
		id := Key("answer", w.ID, q.ID, now, text)
		previous, err := u.GetTrigger(id)
		if err != nil || previous != nil {
			return err
		}
		origin, err := u.GetTrigger(w.Origin.TriggerID)
		if err != nil {
			return err
		}
		if origin == nil || len(origin.Threads) == 0 {
			return fmt.Errorf("起源事件 %q 缺少对象", w.Origin.TriggerID)
		}
		tr := Trigger{ID: id, Source: "craft", Event: "person.answer", Threads: []ThreadRef{origin.Threads[0]}, Actor: ActorRef{Source: "craft", ID: "person"}, Human: true, Addressed: "direct", At: now, Text: clipChars(text, 4000)}
		if len(text) != len(tr.Text) {
			tr.Meta = map[string]json.RawMessage{"truncated": json.RawMessage("true")}
		}
		if err := u.PutTrigger(tr); err != nil {
			return err
		}
		key := ThreadKey(tr.Threads[0])
		if err := u.LinkTrigger(id, key); err != nil {
			return err
		}
		if err := u.PutDisposition(id, TriggerDisposition{At: now, Fanout: []string{w.ID}}); err != nil {
			return err
		}
		if err := withdrawQuestion(u, q); err != nil {
			return err
		}
		q.AnsweredAt = now
		if err := deliverInput(u, w, tr, false, nil, now); err != nil {
			return err
		}
		if err := emitReceived(u, id, []string{key}, now); err != nil {
			return err
		}
		return wake(u, w)
	})
}
