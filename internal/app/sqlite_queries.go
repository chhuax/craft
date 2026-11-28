package app

import (
	"encoding/json/v2"
	"fmt"

	"github.com/chhuax/craft/internal/engine"
)

func (r engineReader) keys(name, key string) ([]string, error) {
	rows, err := r.read.Lookup(name, key)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, row := range rows {
		keys = append(keys, row.Key)
	}
	return keys, nil
}

func (r engineReader) ThreadKeys(id string) ([]string, error)  { return r.keys("threadKeys", id) }
func (r engineReader) TriggerIDs(key string) ([]string, error) { return r.keys("triggerIDs", key) }
func (r engineReader) SubscribedThreadKeys(id string) ([]string, error) {
	return r.keys("subscriptions", id)
}

func (r engineReader) OpenOriginWorkID(key string) (string, error) {
	rows, err := r.read.Lookup("origin", key)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	return rows[0].Key, nil
}

func (r engineReader) threads(name string, args ...any) ([]engine.Thread, error) {
	rows, err := r.read.Lookup(name, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	result := make([]engine.Thread, len(rows))
	err = decodeDocs(rows, func(i int) any { return &result[i] })
	return result, err
}

func (r engineReader) ActiveThreadsWithPending() ([]engine.Thread, error) {
	return r.threads("pending")
}

func (r engineReader) EvaluatingThreads() ([]engine.Thread, error) { return r.threads("evaluating") }
func (r engineReader) ThreadByEval(id string) (*engine.Thread, error) {
	rows, err := r.threads("eval", id)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func (r engineReader) works(name string, args ...any) ([]engine.Work, error) {
	rows, err := r.read.Lookup(name, args...)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	result := make([]engine.Work, len(rows))
	err = decodeDocs(rows, func(i int) any { return &result[i] })
	return result, err
}

func (r engineReader) OpenWorks() ([]engine.Work, error) { return r.works("open") }
func (r engineReader) RecentOpenWorks(limit int, exclude ...string) ([]engine.Work, error) {
	if limit <= 0 {
		return nil, nil
	}
	raw, err := json.Marshal(exclude)
	if err != nil {
		return nil, fmt.Errorf("编码排除的工作键: %w", err)
	}
	return r.works("recent", string(raw), limit)
}

func (r engineReader) AcceptedByTrigger(ids ...string) (map[string]engine.Acceptance, error) {
	result := make(map[string]engine.Acceptance)
	if len(ids) == 0 {
		return result, nil
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("编码已接事件查询键: %w", err)
	}
	rows, err := r.read.Lookup("accepted", string(raw))
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Value == nil {
			continue
		}
		var pair [2]string
		if err := json.Unmarshal(row.Value, &pair); err != nil {
			return nil, fmt.Errorf("解码事件 %q 的接单信息: %w", row.Key, err)
		}
		result[row.Key] = engine.Acceptance{DecisionID: pair[0], WorkID: pair[1]}
	}
	return result, nil
}

func (r engineReader) DecisionsByThread(key string) ([]engine.Decision, error) {
	rows, err := r.read.Lookup("decisions", key)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	result := make([]engine.Decision, len(rows))
	err = decodeDocs(rows, func(i int) any { return &result[i] })
	return result, err
}

func (r engineReader) EventsSince(rev int64, limit int) ([]engine.Event, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.read.Lookup("events", rev, limit)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	result := make([]engine.Event, len(rows))
	err = decodeDocs(rows, func(i int) any { return &result[i] })
	return result, err
}
