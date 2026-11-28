package repotest

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

// 同一批键在五张表中有各自的值，以检测串表、漏项以及共享的嵌套字段。
type records struct {
	Trigger     *engine.Trigger
	Disposition *engine.TriggerDisposition
	Thread      *engine.Thread
	Work        *engine.Work
	Decision    *engine.Decision
}

func sample(key string) records {
	ref := engine.NewThreadRef("source", "item", key)
	return records{
		Trigger:     &engine.Trigger{ID: key, Threads: []engine.ThreadRef{ref}, Meta: map[string]json.RawMessage{"n": json.RawMessage("9007199254740993")}},
		Disposition: &engine.TriggerDisposition{Fanout: []string{key}, Handbacks: []engine.Handback{{WorkID: key, At: at}}},
		Thread:      &engine.Thread{Key: key, Ref: ref, Pending: []string{"event"}},
		Work: &engine.Work{ID: key, Inputs: []engine.WorkInput{
			{Kind: "trigger", TriggerID: key},
			{Kind: "trigger", TriggerID: key, ReadAt: engine.ReadTime{Present: true}},
			{Kind: "trigger", TriggerID: key, ReadAt: engine.ReadTime{Present: true, Time: at}},
		}},
		Decision: &engine.Decision{ID: key, Reason: []engine.Reason{{Code: engine.ReasonSaid, Values: map[string]string{"text": key}}}},
	}
}

func (v records) put(t *testing.T, u engine.Unit) {
	t.Helper()
	must(t, u.PutTrigger(*v.Trigger))
	must(t, u.PutDisposition(v.Trigger.ID, *v.Disposition))
	must(t, u.PutThread(*v.Thread))
	must(t, u.PutWork(*v.Work))
	must(t, u.PutDecision(*v.Decision))
}

func (v records) mutate() {
	v.Trigger.Meta["n"][0] = '1'
	v.Disposition.Handbacks[0].WorkID = "changed"
	v.Thread.Pending[0] = "changed"
	v.Work.Inputs[0].TriggerID = "changed"
	v.Decision.Reason[0].Values["text"] = "changed"
}

func singles(t *testing.T, r engine.Reader, key string) records {
	t.Helper()
	var v records
	var err error
	v.Trigger, err = r.GetTrigger(key)
	must(t, err)
	v.Disposition, err = r.GetDisposition(key)
	must(t, err)
	v.Thread, err = r.GetThread(key)
	must(t, err)
	v.Work, err = r.GetWork(key)
	must(t, err)
	v.Decision, err = r.GetDecision(key)
	must(t, err)
	return v
}

func batch(t *testing.T, r engine.Reader) map[string]records {
	t.Helper()
	keys := []string{"absent", "b", "a", "a"}
	ts, err := r.GetTriggers(keys...)
	must(t, err)
	ds, err := r.GetDispositions(keys...)
	must(t, err)
	hs, err := r.GetThreads(keys...)
	must(t, err)
	ws, err := r.GetWorks(keys...)
	must(t, err)
	cs, err := r.GetDecisions(keys...)
	must(t, err)
	for _, size := range []int{len(ts), len(ds), len(hs), len(ws), len(cs)} {
		if size != 2 {
			t.Fatalf("批量命中 %d 个键，期望 2 个", size)
		}
	}
	rows := make(map[string]records)
	for _, k := range keys {
		rows[k] = records{ts[k], ds[k], hs[k], ws[k], cs[k]}
	}
	return rows
}

func (v records) check(t *testing.T, want records) {
	t.Helper()
	// 比较持久化形状：必需的空集合可在解码后非 nil，大整数必须保留精度。
	var docs [2]any
	for i, rows := range []records{v, want} {
		raw, err := json.Marshal(rows)
		must(t, err)
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		must(t, d.Decode(&docs[i]))
	}
	if !reflect.DeepEqual(docs[0], docs[1]) {
		t.Fatalf("聚合不一致：得到 %v，期望 %v", docs[0], docs[1])
	}
}

func aggregates(t *testing.T, s engine.Store) {
	ctx := t.Context()
	must(t, s.Atomic(ctx, func(u engine.Unit) error {
		for _, k := range []string{"a", "b"} {
			rows := sample(k)
			rows.put(t, u)
			rows.mutate()
			singles(t, u, k).check(t, sample(k))
		}
		// 重放不能改变第一次记录的外部事实。
		replay := sample("a").Trigger
		replay.Text = "changed"
		return u.PutTrigger(*replay)
	}))
	must(t, s.Read(ctx, func(r engine.Reader) error {
		byKey := batch(t, r)
		byKey["absent"].check(t, records{})
		singles(t, r, "absent").check(t, records{})
		for _, k := range []string{"a", "b"} {
			want := sample(k)
			first := singles(t, r, k)
			first.check(t, want)
			first.mutate()
			byKey[k].check(t, want)
			byKey[k].mutate()
			singles(t, r, k).check(t, want)
		}
		return nil
	}))
}
