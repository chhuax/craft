package alarm

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"
)

// AlarmSpec 描述一次登记。Enabled 为空时默认启用，Startup 请求在首次调度时立即运行。
// Run 可调用 List 或 Trigger，应响应 ctx 取消；回调内不可同步等待本服务停止。
type AlarmSpec struct {
	ID      string
	Every   time.Duration
	At      string
	Jitter  time.Duration
	Timeout time.Duration
	Group   string
	Enabled *bool
	Startup bool
	Run     func(context.Context, RunRequest) error
}

// RunRequest 保留触发原因及原定时刻，避免把 goroutine 的实际启动时间当作计划时间。
type RunRequest struct {
	ID          string
	Reason      string // schedule、startup 或 requested
	ScheduledAt time.Time
}

// Config 在创建时确定总并发上限（必填）与健康阈值（默认 5）。
// Rand 可注入独占的随机源。OnHealth 在越过阈值和恢复时于锁外调用，不同 ID 可并发通知，须及时返回且不可同步等待 Stop。
type Config struct {
	Clock            Clock
	Rand             *rand.Rand
	GlobalLimit      int
	FailureThreshold int
	OnHealth         func(id string, unhealthy bool)
}

// Snapshot 是单个闹钟的值副本。未安排周期时 Next 为零，尚无结果时 LastResult 为空。
type Snapshot struct {
	ID                  string    `json:"id"`
	Next                time.Time `json:"next"`
	LastResult          string    `json:"lastResult"` // success、failed 或 skipped
	LastError           string    `json:"lastError"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	State               string    `json:"state"` // idle、running、backoff、disabled 或 stopped
}

func (spec AlarmSpec) validate() (time.Time, error) {
	switch {
	case spec.ID == "":
		return time.Time{}, fmt.Errorf("闹钟 ID 为空")
	case spec.Run == nil:
		return time.Time{}, fmt.Errorf("闹钟 %q 缺少 Run", spec.ID)
	case spec.Every < 0 || spec.Timeout < 0 || spec.Jitter < 0:
		return time.Time{}, fmt.Errorf("闹钟 %q 的间隔、超时和抖动须为非负数", spec.ID)
	case (spec.Every > 0) == (spec.At != ""):
		return time.Time{}, fmt.Errorf("闹钟 %q 必须指定 Every 和 At 之一", spec.ID)
	}
	if spec.At != "" {
		at, err := time.Parse("15:04", spec.At)
		if err != nil {
			return time.Time{}, fmt.Errorf("闹钟 %q 的 At %q: %w", spec.ID, spec.At, err)
		}
		if at.Format("15:04") != spec.At {
			return time.Time{}, fmt.Errorf("闹钟 %q 的 At %q 须为 HH:MM", spec.ID, spec.At)
		}
		return at, nil
	}
	return time.Time{}, nil
}
