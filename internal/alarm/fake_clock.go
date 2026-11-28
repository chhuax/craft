package alarm

import (
	"testing/synctest"
	"time"
)

// FakeClock 使用 synctest.Test 隔离的虚拟时间。零值可用，须与被测服务处于同一测试内。
type FakeClock struct{ RealClock }

// Advance 拨动虚拟时间，等待已唤醒的 goroutine 返回或再次阻塞。只能由测试的一个 goroutine 调用。
func (c FakeClock) Advance(d time.Duration) {
	// 先等当前工作登记完定时器；中间到期的定时器由 synctest 按虚拟时间逐个时刻推进。
	synctest.Wait()
	if d > 0 {
		<-c.NewTimer(d).C
		synctest.Wait()
	}
}
