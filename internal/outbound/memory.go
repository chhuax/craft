package outbound

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"
)

type memory struct {
	mu      sync.Mutex
	rows    map[string]Result
	sentSeq int64
}

// NewMemory 提供与持久仓储相同的原子状态转移，供独立运行和契约测试使用。
func NewMemory() Outbox { return &memory{rows: make(map[string]Result)} }

func clone(r Result) Result {
	r.Params = bytes.Clone(r.Params)
	r.Receipt = bytes.Clone(r.Receipt)
	r.Preview.Value = bytes.Clone(r.Preview.Value)
	if r.AutoAt != nil {
		r.AutoAt = new(*r.AutoAt)
	}
	return r
}

func (m *memory) Get(ctx context.Context, id string) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("读取外发记录: %w", err)
	}
	return clone(m.rows[id]), nil
}

func (m *memory) Records(ctx context.Context, state State) ([]Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("列举外发记录: %w", err)
	}
	var list []Result
	for _, r := range m.rows {
		if state == "" || r.State == state {
			list = append(list, clone(r))
		}
	}
	return list, nil
}

func (m *memory) Save(ctx context.Context, r *Result, rate RateLimit, clock func() time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("保存外发记录: %w", err)
	}
	if err := m.rows[r.ID].PrepareTransition(r, rate, clock()); err != nil {
		return err
	}
	if r.State == Executing && r.AutoAt != nil {
		n := 0
		for _, v := range m.rows {
			if v.AutoAt != nil && *v.AutoAt > *r.AutoAt-rate.Window.Milliseconds() && *v.AutoAt <= *r.AutoAt {
				n++
				if n >= rate.Max {
					return ErrRateLimited
				}
			}
		}
	}
	if r.State == Done {
		m.sentSeq++
		r.SentSeq = m.sentSeq
	}
	m.rows[r.ID] = clone(*r)
	return nil
}
