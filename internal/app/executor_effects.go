package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/host"
	"github.com/chhuax/craft/internal/store"
)

type executor struct {
	r               *Runtime
	name            string
	drivers         map[string]host.Driver
	health          *daemonHealth
	maxWorkers      int
	launchOrder     func(launchFacts, launchFacts) int
	window, maxWait time.Duration
	mu              sync.Mutex
	reserved        map[string]bool
	states          map[host.Session]sampleState
	titles          map[host.Session]titleSource
}

func newExecutor(r *Runtime, name string, drivers map[string]host.Driver, h *daemonHealth) *executor {
	return &executor{
		r: r, name: name, drivers: drivers, health: h,
		maxWorkers: 3, window: 5 * time.Minute, maxWait: 15 * time.Minute,
		reserved: map[string]bool{}, states: map[host.Session]sampleState{}, titles: map[host.Session]titleSource{},
	}
}

type executorHandler struct {
	x    *executor
	kind string
}

// Kind 返回当前处理器负责的持久队列类型。
func (h executorHandler) Kind() string { return h.kind }

// Quiet 让投递故障通过 Work 卡片呈现。
func (h executorHandler) Quiet() bool { return h.kind == "delivery" }

// Ordered 允许其他 Work 在队首退避时使用空槽。
func (h executorHandler) Ordered() bool { return false }

// Run 按持久化载荷执行，写回队列仍须核对原 effect 的代次。
func (h executorHandler) Run(ctx context.Context, row store.Effect) EffectResult {
	return h.x.run(ctx, row)
}

// Limit 根据最新策略和已交给人的工作重新计算可用名额。
func (h executorHandler) Limit() int {
	if h.kind == "stop" {
		return 4
	}
	if h.x.r.policy.Load().Paused() {
		return 0
	}
	if h.kind == "delivery" {
		return 4
	}
	x := h.x
	x.mu.Lock()
	used, err := x.used(context.Background())
	x.mu.Unlock()
	if err != nil {
		x.health.report("executor", "slots", HealthEntry{"error", err.Error()})
		return 0
	}
	return max(0, x.maxWorkers-len(used))
}

type assignmentRef struct {
	WorkID       string       `json:"workId"`
	AssignmentID string       `json:"assignmentId"`
	Session      host.Session `json:"session,omitempty"`
}

func (x *executor) work(ctx context.Context, id string) (w *engine.Work, err error) {
	err = x.r.repo.Read(ctx, func(r engine.Reader) error {
		var err error
		w, err = r.GetWork(id)
		return err
	})
	return
}

func latest(w *engine.Work) *engine.Assignment {
	if w == nil || w.Ended != nil || len(w.Assignments) == 0 {
		return nil
	}
	return &w.Assignments[len(w.Assignments)-1]
}
func sessionOf(w *engine.Work) host.Session {
	if w.Execution == nil || w.Execution.Session == nil {
		return host.Session{}
	}
	return host.Session(*w.Execution.Session)
}
func confirmed(w *engine.Work) bool {
	return w.Execution != nil && w.Execution.Bootstrapped != nil && *w.Execution.Bootstrapped
}
func (x *executor) works(ctx context.Context, query string) (works []engine.Work, err error) {
	err = x.r.repo.db.View(ctx, func(r store.SQLiteReader) error {
		var err error
		works, err = (engineReader{r}).works(query)
		return err
	})
	return
}

// 调用者持有槽位锁。重新读运行派工，使另一个 CLI 提交的 ready 或提问立即释放槽位。
func (x *executor) used(ctx context.Context) (map[string]bool, error) {
	used := maps.Clone(x.reserved)
	ws, err := x.works(ctx, "executorRunning")
	for i := range ws {
		w := &ws[i]
		if occupiesSlot(w, x.states[sessionOf(w)].state) {
			used[w.ID] = true
		}
	}
	return used, err
}
func occupiesSlot(w *engine.Work, state host.State) bool {
	a := latest(w)
	return state == host.Busy && a != nil && a.State == engine.AssignmentRunning && (a.IdleAt == "" || a.DeliveredAt > a.IdleAt) && w.Mandate.Basis.Via != engine.ViaHumanDirect && engine.WorkStage(*w).Stage != "person"
}

func (x *executor) reserve(ctx context.Context, id string, person bool) error {
	x.mu.Lock()
	defer x.mu.Unlock()
	used, err := x.used(ctx)
	if err != nil {
		return err
	}
	limit := x.maxWorkers
	if person {
		limit++
	}
	if x.reserved[id] || !used[id] && len(used) >= limit {
		return host.Later
	}
	x.reserved[id] = true
	return nil
}
func (x *executor) free(id string) { x.mu.Lock(); defer x.mu.Unlock(); delete(x.reserved, id) }

// 即使两次送达同毫秒且宿主都忙，也要丢弃在新送达前取得的旧样本。
type sampleState struct {
	state   host.State
	receipt uint64
}

func (x *executor) setState(s host.Session, state host.State) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.states[s] = sampleState{state: state, receipt: x.states[s].receipt + 1}
}

var errStale = errors.New("派工已更换或结束")

func (x *executor) current(ctx context.Context, p assignmentRef) (*engine.Work, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if x.r.policy.Load().Paused() {
		return nil, host.Later
	}
	var w *engine.Work
	err := x.r.repo.Read(ctx, func(r engine.Reader) error {
		var err error
		w, err = r.GetWork(p.WorkID)
		if err != nil {
			return err
		}
		a := latest(w)
		if a == nil || a.ID != p.AssignmentID || a.State == engine.AssignmentEnded || w.Delivery != nil || w.Mandate.Basis.Via == engine.ViaHumanDirect {
			return errStale
		}
		for _, old := range w.Assignments {
			ok, err := r.HasEffect("stop", old.ID)
			if err != nil {
				return err
			}
			if ok {
				return host.Later
			}
		}
		return nil
	})
	return w, err
}
func (x *executor) run(ctx context.Context, row store.Effect) EffectResult {
	var p assignmentRef
	if err := json.Unmarshal(row.Value, &p); err != nil {
		return EffectResult{Err: fmt.Errorf("解析 %s 载荷: %w", row.Kind, err)}
	}
	var err error
	switch row.Kind {
	case "stop":
		err = x.stop(ctx, p, row.Attempts)
	case "launch":
		err = x.launch(ctx, p, row.Attempts)
	case "delivery":
		return x.deliver(ctx, p.WorkID)
	}
	return effectOutcome(err)
}
func effectOutcome(err error) EffectResult {
	switch {
	case errors.Is(err, errStale):
		return EffectResult{}
	case errors.Is(err, errTransient):
		return EffectResult{Err: err}
	case errors.Is(err, host.Later):
		return EffectResult{Later: true}
	default:
		return EffectResult{Err: err}
	}
}
func (x *executor) stop(ctx context.Context, p assignmentRef, attempts int) error {
	d := x.drivers[p.Session.Host]
	err := error(host.Later)
	if d != nil {
		err = d.Stop(ctx, p.Session, 10*time.Second)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil || d != nil && d.Status(ctx, p.Session) == host.Gone {
		x.mu.Lock()
		delete(x.states, p.Session)
		delete(x.titles, p.Session)
		x.mu.Unlock()
		return x.r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
			return tx.Delete("watch_kv", "stopping."+p.Session.Host+"."+p.Session.ID)
		})
	}
	if errors.Is(err, host.Later) {
		return host.Later
	}
	if !errors.Is(err, host.Unknown) {
		return nil
	}
	if attempts < 2 {
		return fmt.Errorf("停止会话 %s: %w", p.Session.ID, err)
	}
	x.health.report("executor", "stop."+p.AssignmentID, HealthEntry{"error", fmt.Sprintf("会话 %s 停止未确认: %v", p.Session.ID, err)})
	return nil
}
