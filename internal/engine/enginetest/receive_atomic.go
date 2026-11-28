package enginetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

// 在真实事务端口处观察入队与批读，持久化适配器可原样运行故障用例。
type intakeStore struct {
	engine.Store
	writing                    bool
	transactions, triggerReads int
	threadReads, workReads     [][]string
	enqueued                   []string
	staged                     []string
	failKey                    string
}

type intakeUnit struct {
	engine.Unit
	store *intakeStore
}

func (s *intakeStore) Atomic(ctx context.Context, fn func(engine.Unit) error) error {
	s.transactions++
	return s.Store.Atomic(ctx, func(u engine.Unit) error {
		s.writing = true
		defer func() { s.writing = false }()
		return fn(intakeUnit{u, s})
	})
}

func (u intakeUnit) GetTrigger(id string) (*engine.Trigger, error) {
	u.store.triggerReads++
	return u.Unit.GetTrigger(id)
}

func (u intakeUnit) GetThreads(keys ...string) (map[string]*engine.Thread, error) {
	u.store.threadReads = append(u.store.threadReads, slices.Clone(keys))
	return u.Unit.GetThreads(keys...)
}

func (u intakeUnit) GetWorks(ids ...string) (map[string]*engine.Work, error) {
	u.store.workReads = append(u.store.workReads, slices.Clone(ids))
	return u.Unit.GetWorks(ids...)
}

func (u intakeUnit) Emit(ev engine.Event) error {
	if err := u.Unit.Emit(ev); err != nil {
		return err
	}
	u.store.staged = append(u.store.staged, ev.Type)
	return nil
}

func (u intakeUnit) Effects() engine.Effects { return u }

var errQueue = errors.New("投递队列故障")

func (u intakeUnit) Enqueue(kind, key string, value json.RawMessage) error {
	var payload struct {
		WorkID string `json:"workId"`
	}
	if err := json.Unmarshal(value, &payload); err != nil {
		return err
	}
	if kind != "delivery" || key != payload.WorkID {
		return fmt.Errorf("意外的投递请求 %s %s %s", kind, key, value)
	}
	u.store.enqueued = append(u.store.enqueued, key)
	if key == u.store.failKey {
		return errQueue
	}
	return u.Unit.Effects().Enqueue(kind, key, value)
}

type intakeState struct {
	trigger     *engine.Trigger
	disposition *engine.TriggerDisposition
	threads     map[string]*engine.Thread
	works       map[string]*engine.Work
	keys        []string
	events      []engine.Event
}

func readIntake(t *testing.T, s engine.Store, id string, keys, ids []string) intakeState {
	t.Helper()
	var state intakeState
	must(t, s.Read(t.Context(), func(r engine.Reader) error {
		var err error
		state.trigger, err = r.GetTrigger(id)
		must(t, err)
		state.disposition, err = r.GetDisposition(id)
		must(t, err)
		state.threads, err = r.GetThreads(keys...)
		must(t, err)
		state.works, err = r.GetWorks(ids...)
		must(t, err)
		state.keys, err = r.ThreadKeys(id)
		must(t, err)
		state.events, err = r.EventsSince(0, 20)
		return err
	}))
	return state
}

func receiveAtomic(t *testing.T, s engine.Store) {
	a := engine.NewThreadRef("feed", "item", "a")
	b := engine.NewThreadRef("feed", "item", "b")
	c := engine.NewThreadRef("feed", "item", "new")
	tr := trigger("one", "notice", a, b, a, c)
	tr.Human = true
	keys := []string{engine.ThreadKey(a), engine.ThreadKey(b), engine.ThreadKey(c)}
	ids := []string{"first", "second"}
	must(t, s.Atomic(t.Context(), func(u engine.Unit) error {
		must(t, u.PutThread(engine.Thread{Ref: a, Key: keys[0], Subscriptions: ids, Context: ids}))
		must(t, u.PutThread(engine.Thread{Ref: b, Key: keys[1], Subscriptions: ids[:1]}))
		must(t, u.PutWork(engine.Work{ID: ids[0], Ended: &engine.WorkEnd{By: "executor"}, Mandate: engine.Mandate{Strength: engine.Explicit, Basis: engine.MandateBasis{Source: "feed", Event: "change"}}}))
		return u.PutWork(engine.Work{ID: ids[1]})
	}))
	store := &intakeStore{Store: s, failKey: ids[1]}
	e := newEngine(t, receiveConfig(t, store))
	var broadcast []engine.Event
	s.Subscribe(func(ev engine.Event) { broadcast = append(broadcast, ev) })
	before := readIntake(t, s, tr.ID, keys, ids)
	got, err := e.Receive(t.Context(), tr)
	if !errors.Is(err, errQueue) || got != "" || !slices.Equal(store.enqueued, ids) || !slices.Contains(store.staged, "work.resumed") {
		t.Fatalf("第二次入队应令整条命令失败，得到 %q %v，入队 %v", got, err, store.enqueued)
	}
	if after := readIntake(t, s, tr.ID, keys, ids); !reflect.DeepEqual(before, after) || len(broadcast) != 0 {
		t.Fatalf("故障后状态或广播发生变化 %+v %v", after, broadcast)
	}

	store.failKey = ""
	receive(t, e, tr, engine.Stored)
	committed := readIntake(t, s, tr.ID, keys, ids)
	d := committed.disposition
	if d == nil || !slices.Equal(d.Fanout, ids) || !slices.Equal(d.Context, ids[1:]) || !slices.Equal(store.enqueued, []string{"first", "second", "first", "second"}) {
		t.Fatalf("重试后分发 %v、入队 %v", d, store.enqueued)
	}
	if len(committed.works[ids[0]].Inputs) != 2 || len(committed.works[ids[1]].Inputs) != 1 || len(committed.threads[keys[0]].Pending) != 0 {
		t.Fatalf("重复路径或纯上下文影响了认领结果 %+v", committed)
	}
	if len(broadcast) != 2 || !reflect.DeepEqual(committed.events, broadcast) || broadcast[0].Type != "work.resumed" || broadcast[0].Ref["work"] != ids[0] ||
		broadcast[1].Type != "trigger.received" || broadcast[1].Ref["trigger"] != tr.ID || broadcast[1].Ref["thread"] != keys[0] {
		t.Fatalf("提交后的广播或事件日志异常 %v %v", broadcast, committed.events)
	}

	tr.Text = "重复事件携带的新正文"
	receive(t, e, tr, engine.Duplicate)
	if after := readIntake(t, s, tr.ID, keys, ids); !reflect.DeepEqual(committed, after) || len(broadcast) != 2 || len(store.enqueued) != 4 {
		t.Fatalf("重复接收改变了状态、广播或入队 %+v %v %v", after, broadcast, store.enqueued)
	}
	if store.transactions != 3 || store.triggerReads != 3 || !reflect.DeepEqual(store.threadReads, [][]string{keys, keys}) || !reflect.DeepEqual(store.workReads, [][]string{ids, ids}) {
		t.Fatalf("命令事务或按键读取次数异常 %d %d %v %v", store.transactions, store.triggerReads, store.threadReads, store.workReads)
	}
}
