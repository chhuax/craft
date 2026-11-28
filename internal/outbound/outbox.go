package outbound

import (
	"cmp"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

// Show 按外发记录 ID（亦即幂等键）取回完整预览、状态与回执。
func (s *Service) Show(ctx context.Context, id string) (Result, error) {
	r, err := s.box.Get(ctx, id)
	if err != nil {
		return Result{}, fmt.Errorf("读取外发记录 %q: %w", id, err)
	}
	if r.ID == "" {
		return Result{}, fmt.Errorf("外发记录 %q 不存在", id)
	}
	return r, nil
}

// List 按最近变化在前列出记录，时间相同时按 ID 排序。
func (s *Service) List(ctx context.Context) ([]Result, error) { return s.list(ctx, "") }

func (s *Service) list(ctx context.Context, state State) ([]Result, error) {
	rows, err := s.box.Records(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("查询外发记录 %q: %w", state, err)
	}
	slices.SortFunc(rows, func(a, b Result) int { return cmp.Or(b.At.Compare(a.At), cmp.Compare(a.ID, b.ID)) })
	return rows, nil
}

// Confirmation 为待办合并提供当前确认项的定位与展示字段。
type Confirmation struct {
	ID, Action, Preview string
	Source              Source
	At                  time.Time
}

// PendingConfirmations 只读取待确认状态，确认和撤回随事务提交即从清单消失。
func (s *Service) PendingConfirmations(ctx context.Context) ([]Confirmation, error) {
	rows, err := s.list(ctx, Pending)
	if err != nil {
		return nil, err
	}
	items := make([]Confirmation, 0, len(rows))
	for _, r := range rows {
		items = append(items, Confirmation{r.ID, r.Action, r.Preview.Summary, r.Source, r.At})
	}
	return items, nil
}

// Confirm 由显式确认操作调用，每次只处理一条待办并重新预览其原始请求。
func (s *Service) Confirm(ctx context.Context, id, named string) (Result, error) {
	r, err := s.Show(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if r.State != Pending {
		return r, fmt.Errorf("外发记录 %q 不在待确认清单中: %w", id, ErrConflict)
	}
	return s.preview(ctx, r, s.action(r.Action), true, named)
}

// Withdraw 只允许撤回仍待确认的记录，同时进行的确认由仓储前态检查裁决。
func (s *Service) Withdraw(ctx context.Context, id string) error {
	r, err := s.Show(ctx, id)
	if err != nil {
		return err
	}
	if r.State != Pending {
		return fmt.Errorf("外发记录 %q 不可撤回: %w", id, ErrConflict)
	}
	_, err = s.move(ctx, r, Withdrawn)
	return err
}

// Recover 仅在先前的发送者已停止后调用，普通重复提交不能据执行中状态推断崩溃。
// 核实永不触发重发；单项无法核实时记录结果未知并继续核实其他项。
func (s *Service) Recover(ctx context.Context) error {
	rows, err := s.box.Records(ctx, Executing)
	if err != nil {
		return fmt.Errorf("查找中断的发送: %w", err)
	}
	var failures []error
	for _, r := range rows {
		if ctx.Err() != nil {
			return errors.Join(append(failures, ctx.Err())...)
		}
		status, receipt, verifyErr := s.ext.Verify(ctx, protocol.ActionVerifyParams{Action: r.Action, IdempotencyKey: r.ID})
		next := Unknown
		if verifyErr == nil && receipt != nil && !jsontext.Value(receipt).IsValid() {
			verifyErr = errors.New("扩展的核实回执不是有效 JSON")
		}
		if verifyErr != nil {
			r.Detail = verifyErr.Error()
			failures = append(failures, fmt.Errorf("核实外发记录 %q: %w", r.ID, verifyErr))
		} else {
			r.Receipt = receipt
			switch status {
			case Applied:
				next = Done
			case NotApplied:
				next = Failed
			}
		}
		if _, err := s.move(ctx, r, next); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
