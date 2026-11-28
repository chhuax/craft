package alarm

import (
	"slices"
	"testing"
	"testing/synctest"
	"time"
)

func TestFakeClockAdvance(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := FakeClock{}
		start := c.Now()
		fired := make(chan int, 3)
		go func() {
			<-c.NewTimer(2 * time.Second).C
			fired <- 2
			<-c.NewTimer(time.Second).C
			fired <- 3
		}()
		go func() {
			<-c.NewTimer(time.Second).C
			fired <- 1
		}()
		c.Advance(4 * time.Second)
		close(fired)
		var order []int
		for n := range fired {
			order = append(order, n)
		}
		if !slices.Equal(order, []int{1, 2, 3}) || c.Now().Sub(start) != 4*time.Second {
			t.Fatalf("拨钟后顺序 %v，经过 %v", order, c.Now().Sub(start))
		}
	})
}
