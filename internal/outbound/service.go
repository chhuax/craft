package outbound

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

// Config 由应用层提供动作目录、组织下限、窗口和时钟。Content 为 nil 时采用内置规则。
type Config struct {
	Actions      []Action
	Organization map[string]Level
	Outbox       Outbox
	Executor     Executor
	Content      ContentRules
	Rate         RateLimit
	Clock        func() time.Time
}

// Service 按级别编排预览、确认与发送；扩展调用始终在存储事务之外。
type Service struct {
	actions map[string]Action
	box     Outbox
	ext     Executor
	content ContentRules
	rate    RateLimit
	now     func() time.Time
}

// New 校验动作声明并取组织规则与声明两者中的较严等级。
func New(cfg Config) (*Service, error) {
	if cfg.Outbox == nil || cfg.Executor == nil || cfg.Clock == nil || !cfg.Rate.valid() {
		return nil, errors.New("外发服务需要仓储、扩展、时钟与正数毫秒频率窗口")
	}
	s := &Service{
		actions: make(map[string]Action), box: cfg.Outbox, ext: cfg.Executor,
		content: slices.Clone(cfg.Content), rate: cfg.Rate, now: cfg.Clock,
	}
	if s.content == nil {
		s.content = DefaultContentRules()
	}
	for _, rule := range s.content {
		if rule == nil {
			return nil, errors.New("空内容规则")
		}
	}
	for _, a := range cfg.Actions {
		valid := slices.Contains(levels, a.Level) || a.ReadOnly && a.Level == ""
		if a.ID == "" || !valid {
			return nil, fmt.Errorf("动作 %q 的声明无效，级别 %q", a.ID, a.Level)
		}
		if _, ok := s.actions[a.ID]; ok {
			return nil, fmt.Errorf("动作重复 %q", a.ID)
		}
		s.actions[a.ID] = a
	}
	for id, level := range cfg.Organization {
		if id == "" || !slices.Contains(levels, level) {
			return nil, fmt.Errorf("动作 %q 的组织级别无效 %q", id, level)
		}
		a := s.action(id)
		a.Level = stricter(a.Level, level)
		s.actions[id] = a
	}
	return s, nil
}

func (s *Service) action(id string) Action {
	if a, ok := s.actions[id]; ok {
		return a
	}
	return Action{ID: id, Level: Confirm}
}

// Act 对新请求现查预览，按登记的等级转发。相同幂等键只能用于原动作与参数。
func (s *Service) Act(ctx context.Context, q Request) (Result, error) {
	a := s.action(q.Action)
	if a.Level == Deny {
		return Result{}, fmt.Errorf("动作 %q: %w", q.Action, ErrDenied)
	}
	if q.Params == nil {
		q.Params = json.RawMessage("null")
	}
	if q.Action == "" || !jsontext.Value(q.Params).IsValid() {
		return Result{}, errors.New("动作为空或参数不是有效 JSON")
	}
	if a.ReadOnly {
		reply, err := s.call(ctx, Result{ID: q.IdempotencyKey, Action: q.Action, Params: q.Params}, false)
		if err != nil {
			return Result{}, fmt.Errorf("读取动作 %q: %w", q.Action, err)
		}
		return Result{Action: q.Action, State: Done, Receipt: reply.Value}, nil
	}
	if q.IdempotencyKey == "" || q.Source != Worker && q.Source != Session {
		return Result{}, errors.New("外发请求缺少幂等键或有效来源")
	}
	r, err := s.box.Get(ctx, q.IdempotencyKey)
	if err != nil {
		return Result{}, fmt.Errorf("查找幂等键 %q: %w", q.IdempotencyKey, err)
	}
	if r.ID == "" {
		r = Result{ID: q.IdempotencyKey, Action: q.Action, Params: q.Params, Source: q.Source, Level: a.Level, Reversible: a.Reversible}
		r, err = s.move(ctx, r, Previewing)
		if errors.Is(err, ErrConflict) {
			// 同键并发首次提交时，输掉插入的一方必须按已提交的身份和状态处理。
			r, err = s.Show(ctx, q.IdempotencyKey)
		}
		if err != nil {
			return Result{}, err
		}
	}
	if r.Action != q.Action || !sameParams(r.Params, q.Params) {
		return r, fmt.Errorf("幂等键 %q 已绑定其他请求: %w", r.ID, ErrConflict)
	}
	switch r.State {
	case Done:
		return r, nil
	case Previewing, Pending, Confirmed:
		return s.preview(ctx, r, a, q.Yes && q.Source != Worker, q.NamedTarget)
	default:
		return r, fmt.Errorf("外发记录 %q 处于 %s: %w", r.ID, r.State, ErrConflict)
	}
}

func sameParams(a, b json.RawMessage) bool {
	if bytes.Equal(a, b) {
		return true
	}
	// 允许持久化时的排版与字符串转义差异，不能经浮点数比较大整数。
	x, y := jsontext.Value(a).Clone(), jsontext.Value(b).Clone()
	opts := jsontext.ReorderRawObjects(true)
	return x.Format(opts) == nil && y.Format(opts) == nil && bytes.Equal(x, y)
}

func (s *Service) preview(ctx context.Context, r Result, a Action, yes bool, named string) (Result, error) {
	// 已因内容或窗口转入确认的记录，即使窗口滑过也不能被无确认的重放发出。
	r.Level = stricter(r.Level, a.Level)
	if r.Level == Deny {
		return r, fmt.Errorf("动作 %q: %w", r.Action, ErrDenied)
	}
	reply, err := s.call(ctx, r, true)
	if err != nil {
		return s.callFailure(ctx, r, err)
	}
	r.Preview = reply
	r.Reversible = a.Reversible
	if r.Level == Auto {
		hit, err := s.inspect(r)
		if err != nil {
			return r, err
		}
		if !hit {
			start, err := s.move(ctx, r, Executing)
			if err == nil {
				return s.send(ctx, start)
			}
			if !errors.Is(err, ErrRateLimited) {
				return r, err
			}
		}
		r.Level = Confirm
	}
	if !yes {
		return s.wait(ctx, r, NeedsConfirmation)
	}
	if r.Level == ConfirmNamed && (strings.TrimSpace(named) == "" || named != r.Preview.Target) {
		return s.wait(ctx, r, ErrDenied)
	}
	if r.State != Confirmed {
		r, err = s.move(ctx, r, Confirmed)
		if err != nil {
			return r, err
		}
	}
	r, err = s.move(ctx, r, Executing)
	if err != nil {
		return r, err
	}
	return s.send(ctx, r)
}

func (s *Service) inspect(r Result) (bool, error) {
	if s.content.Matches(r.Preview.Summary) || s.content.Matches(r.Preview.Target) {
		return true, nil
	}
	for _, raw := range []json.RawMessage{r.Params, r.Preview.Value} {
		hit, err := s.content.checkJSON(raw)
		if err != nil || hit {
			return hit, err
		}
	}
	return false, nil
}

func (s *Service) wait(ctx context.Context, r Result, reason error) (Result, error) {
	r, err := s.move(ctx, r, Pending)
	if err != nil {
		return r, err
	}
	return r, fmt.Errorf("外发记录 %q: %w", r.ID, reason)
}

func (s *Service) call(ctx context.Context, r Result, preview bool) (Reply, error) {
	reply, err := s.ext.Execute(ctx, protocol.ActionExecuteParams{Action: r.Action, Params: r.Params, Preview: preview, IdempotencyKey: r.ID})
	if err != nil {
		return Reply{}, fmt.Errorf("扩展执行 %q (preview=%t): %w", r.ID, preview, err)
	}
	if reply.Value != nil && !jsontext.Value(reply.Value).IsValid() {
		return Reply{}, fmt.Errorf("扩展动作 %q 的返回值不是有效 JSON", r.Action)
	}
	return reply, nil
}

func (s *Service) send(ctx context.Context, r Result) (Result, error) {
	reply, err := s.call(ctx, r, false)
	if err != nil {
		return s.callFailure(ctx, r, err)
	}
	r.Receipt = reply.Value
	return s.move(ctx, r, Done)
}

func (s *Service) callFailure(ctx context.Context, r Result, err error) (Result, error) {
	if !errors.Is(err, ErrDenied) {
		// 断连和取消都可能晚于扩展生效。执行中须留给恢复核实；预览中可安全重试。
		return r, err
	}
	r.Detail = err.Error()
	failed, saveErr := s.move(ctx, r, Failed)
	return failed, errors.Join(err, saveErr)
}

func (s *Service) move(ctx context.Context, r Result, to State) (Result, error) {
	next := r
	next.Revision++
	next.State = to
	if err := s.box.Save(ctx, &next, s.rate, s.now); err != nil {
		return r, fmt.Errorf("保存外发记录 %q (%s → %s): %w", r.ID, r.State, to, err)
	}
	return next, nil
}
