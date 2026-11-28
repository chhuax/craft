package app

import (
	"context"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/store"
)

func benchEffectKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = strconv.Itoa(i)
	}
	return keys
}

// 用健康回调确认队列已回到基线；处理器返回时，行的删除事务可能还没提交。
func benchEffectLayer(b *testing.B, db *store.SQLite, h *testEffectHandler, baseline int) (EffectLayer, <-chan struct{}) {
	b.Helper()
	settled := make(chan struct{}, 1)
	layer, err := NewEffectScheduler(db, alarm.RealClock{}, time.Second, time.Minute, func(_ string, state HealthState) {
		if state.Waiting == baseline {
			settled <- struct{}{}
		}
	}, h)
	effectOK(b, err)
	effectOK(b, layer.Start(b.Context()))
	b.Cleanup(func() { effectOK(b, layer.Stop(context.Background())) })
	<-settled
	return layer, settled
}

// BenchmarkEffectKick 从外部 Kick 计到十个到期处理器全部进入 Run；入队和写回在计时区间外。
func BenchmarkEffectKick(b *testing.B) {
	for _, future := range []int{0, 9990} {
		b.Run(fmt.Sprintf("队列_%d", future+10), func(b *testing.B) {
			clock := alarm.RealClock{}
			db := effectDB(b, clock.Now)
			h := &testEffectHandler{kind: "bench"}
			h.limit.Store(10)
			keys := benchEffectKeys(future + 10)
			enqueueEffects(b, db, h.kind, keys[10:]...)
			now := clock.Now()
			effectOK(b, db.Write(b.Context(), func(tx *store.SQLiteTx) error {
				rows, err := tx.DueEffects(h.kind, now, future)
				if err != nil {
					return err
				}
				if len(rows) != future {
					b.Fatalf("基准积压 %d 行", len(rows))
				}
				for _, row := range rows {
					if err := tx.LaterEffect(row, now, 24*time.Hour); err != nil {
						return err
					}
				}
				return nil
			}))
			started, release := make(chan struct{}, 10), make(chan struct{}, 10)
			h.run = func(ctx context.Context, _ store.Effect) EffectResult {
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return EffectResult{}
			}
			layer, settled := benchEffectLayer(b, db, h, future)
			b.ReportAllocs()
			for b.Loop() {
				b.StopTimer()
				enqueueEffects(b, db, h.kind, keys[:10]...)
				b.StartTimer()
				layer.Kick()
				for range 10 {
					<-started
				}
				b.StopTimer()
				for range 10 {
					release <- struct{}{}
				}
				<-settled
				b.StartTimer()
			}
		})
	}
}

// BenchmarkEffectThroughput 包含一次事务入队一千行、调度执行以及逐行持久化删除。
func BenchmarkEffectThroughput(b *testing.B) {
	const count = 1000
	for _, limit := range []int32{1, 4, 16} {
		b.Run(fmt.Sprintf("Limit_%d", limit), func(b *testing.B) {
			db := effectDB(b, alarm.RealClock{}.Now)
			h := &testEffectHandler{kind: "bench", run: func(context.Context, store.Effect) EffectResult { return EffectResult{} }}
			h.limit.Store(limit)
			layer, settled := benchEffectLayer(b, db, h, 0)
			keys := benchEffectKeys(count)
			for b.Loop() {
				enqueueEffects(b, db, h.kind, keys...)
				layer.Kick()
				<-settled
			}
			b.ReportMetric(float64(b.N)*count/b.Elapsed().Seconds(), "events/s")
		})
	}
}
