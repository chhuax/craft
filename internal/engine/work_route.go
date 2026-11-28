package engine

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// 调用方已在写事务中读出可关闭对象。只沿订阅查询潜在持有人，
// 不能用 Work 的阶段代替 ended，也不能把纯跟随或工作自身视作冲突。
func checkOwnAvailable(r Reader, threads []*Thread, workID string, works map[string]*Work) error {
	var ids []string
	for _, th := range threads {
		for _, id := range th.Subscriptions {
			if _, loaded := works[id]; !loaded && id != workID {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) > 0 {
		found, err := r.GetWorks(uniqueIDs(ids)...)
		if err != nil {
			return err
		}
		if works == nil {
			works = found
		} else {
			maps.Copy(works, found)
		}
	}
	for _, th := range threads {
		for _, id := range th.Subscriptions {
			w := works[id]
			if w == nil || id == workID || w.Ended != nil {
				continue
			}
			if slices.ContainsFunc(w.Follows, func(f Follow) bool { return f.Own && f.Thread == th.Key }) {
				return &UserError{Code: CodeThreadAlreadyOwned, Values: map[string]string{"threads": th.Key}}
			}
		}
	}
	return nil
}

// 主对象必须复用命令持有的聚合，避免后续清除 evaluating 时覆盖刚登记的订阅。
func (e *Engine) routeWork(u Unit, w *Work, main *Thread, batch []*Trigger, now string, resuming bool, knownWorks map[string]*Work) (map[string]*Thread, error) {
	if e.cfg.Scope == nil {
		return nil, errors.New("登记自有对象需要 SourceScope")
	}
	var refs []ThreadRef
	via := map[string][]string{}
	for _, tr := range batch {
		refs = append(refs, tr.Threads...)
		primary := tr.Threads[0]
		if e.cfg.Scope.Closable(primary.Source(), primary.Kind()) {
			key := ThreadKey(primary)
			via[key] = append(via[key], tr.ID)
		}
	}
	keys := triggerThreadKeys(refs)
	loadKeys := slices.Clone(keys)
	if resuming {
		// 恢复会同时重新占用历史自有对象，即使本次事件只提到了一个新对象。
		for _, f := range w.Follows {
			loadKeys = append(loadKeys, f.Thread)
		}
	}
	loadKeys = slices.DeleteFunc(uniqueIDs(loadKeys), func(key string) bool { return key == main.Key })
	threads, err := u.GetThreads(loadKeys...)
	if err != nil {
		return nil, err
	}
	if threads == nil {
		threads = make(map[string]*Thread)
	}
	threads[main.Key] = main
	for _, ref := range refs {
		key := ThreadKey(ref)
		threads[key] = threadOrNew(threads[key], ref, now)
	}
	var owned []*Thread
	for _, key := range keys {
		if len(via[key]) != 0 {
			owned = append(owned, threads[key])
		}
	}
	if resuming {
		owned = append(owned, closableFollows(w, threads, e.cfg.Scope)...)
	}
	if err := checkOwnAvailable(u, owned, w.ID, knownWorks); err != nil {
		return nil, err
	}
	for _, key := range keys {
		th := threads[key]
		if err := subscribeWork(u, th, w.ID, now); err != nil {
			return nil, err
		}
		if triggers := via[key]; len(triggers) != 0 {
			addOwnFollow(w, th, triggers)
		}
	}
	return threads, nil
}

func closableFollows(w *Work, threads map[string]*Thread, scope SourceScope) []*Thread {
	var owned []*Thread
	for _, f := range w.Follows {
		th := threads[f.Thread]
		if f.Own && th != nil && scope.Closable(th.Ref.Source(), th.Ref.Kind()) {
			owned = append(owned, th)
		}
	}
	return owned
}

func subscribeWork(u Unit, th *Thread, id, now string) error {
	existing := slices.Contains(th.Subscriptions, id)
	if existing && !slices.Contains(th.Context, id) {
		return nil
	}
	if !existing {
		th.Subscriptions = append(th.Subscriptions, id)
	}
	th.Context = slices.DeleteFunc(th.Context, func(k string) bool { return k == id })
	if err := u.PutThread(*th); err != nil {
		return err
	}
	if existing {
		return nil
	}
	return emitAssessment(u, "thread.subscribed", now, map[string]string{"thread": th.Key, "work": id}, map[string]any{"threadKey": th.Key, "workId": id})
}

func addOwnFollow(w *Work, th *Thread, ids []string) {
	for i := range w.Follows {
		f := &w.Follows[i]
		if f.Thread == th.Key {
			f.Own = true
			for _, id := range ids {
				if !slices.Contains(f.Via, id) {
					f.Via = append(f.Via, id)
				}
			}
			return
		}
	}
	w.Follows = append(w.Follows, Follow{Thread: th.Key, Own: true, Closed: th.Closed, Via: ids})
}

func launchWork(u Unit, w *Work, batch []*Trigger, now string) error {
	// 地址允许嵌套扩展字段，比较规范 JSON 才能把键顺序不同的相同渠道合并。
	seen := map[string]bool{}
	for _, tr := range batch {
		if tr.Channel == nil {
			continue
		}
		raw, err := jsonv2.Marshal(tr.Channel)
		if err != nil {
			return fmt.Errorf("编码回复渠道: %w", err)
		}
		address := jsontext.Value(raw)
		if err := address.Canonicalize(jsontext.CanonicalizeRawInts(false), jsontext.CanonicalizeRawFloats(false)); err != nil {
			return fmt.Errorf("规范化回复渠道: %w", err)
		}
		key := string(address)
		if !seen[key] {
			w.Channels = append(w.Channels, *tr.Channel)
			seen[key] = true
		}
	}
	a := Assignment{ID: Key("assignment", w.ID, "0"), State: AssignmentPending, CreatedAt: now}
	w.Assignments = []Assignment{a}
	if err := u.PutWork(*w); err != nil {
		return err
	}
	if err := emitAssessment(u, "work.created", now, map[string]string{"work": w.ID, "decision": w.DecisionID, "thread": ThreadKey(batch[0].Threads[0])},
		map[string]any{"workId": w.ID, "decisionId": w.DecisionID, "strength": w.Mandate.Strength}); err != nil {
		return err
	}
	return enqueueLaunch(u, w.ID, a.ID)
}
