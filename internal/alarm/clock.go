package alarm

import "time"

// Clock 为周期、退避与超时提供同一条时间线。实现必须支持并发调用。
type Clock interface {
	Now() time.Time
	NewTimer(time.Duration) *time.Timer
}

// RealClock 使用系统时钟。零值可供多个服务共用。
type RealClock struct{}

// Now 返回当前本地时间，可并发调用。
func (RealClock) Now() time.Time { return time.Now() }

// NewTimer 创建一次性定时器，可并发调用；调用方负责停止提前弃用的定时器。
func (RealClock) NewTimer(d time.Duration) *time.Timer { return time.NewTimer(d) }
