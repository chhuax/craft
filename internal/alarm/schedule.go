package alarm

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"time"
)

func (s *Service) planFirst(e *entry, now time.Time) {
	if !e.enabled {
		return
	}
	if e.startup {
		e.snap.Next = now
		return
	}
	s.planRegular(e, now)
}

func (s *Service) planRegular(e *entry, now time.Time) {
	if e.spec.Every != 0 {
		d := e.spec.Every
		if !e.nominal.IsZero() {
			// 调度被延迟时只处理当前这一拍，跳过已经过去的节拍。
			d -= now.Sub(e.nominal) % d
		}
		e.nominal = now.Add(d)
	} else {
		e.nominal = nextDaily(now, e.at)
	}
	e.snap.Next = e.nominal
	if e.spec.Jitter > 0 {
		draw := rand.Uint64N
		if s.cfg.Rand != nil {
			draw = s.cfg.Rand.Uint64N
		}
		e.snap.Next = e.nominal.Add(time.Duration(draw(uint64(e.spec.Jitter) + 1)))
	}
}

func nextDaily(now, at time.Time) time.Time {
	day := now.Local()
	for {
		y, m, d := day.Date()
		next := time.Date(y, m, d, at.Hour(), at.Minute(), 0, 0, day.Location())
		// 夏令时跳过的本地时刻没有对应的瞬间；按日历找下一天，不能固定加 24 小时。
		if next.Hour() == at.Hour() && next.Minute() == at.Minute() && next.After(now) {
			return next
		}
		day = time.Date(y, m, d+1, 12, 0, 0, 0, day.Location())
	}
}

func (s *Service) loop() {
	defer close(s.done)
	for {
		s.mu.Lock()
		if s.stopping() {
			active := s.active
			s.mu.Unlock()
			if active == 0 {
				return
			}
			<-s.wake
			continue
		}
		clock := s.cfg.Clock
		next := s.dispatch(clock.Now())
		var timer *time.Timer
		var tick <-chan time.Time
		if !next.IsZero() {
			timer = clock.NewTimer(next.Sub(clock.Now()))
			tick = timer.C
		}
		s.mu.Unlock()
		select {
		case <-s.ctx.Done():
		case <-s.wake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// dispatch 在同一把锁内选取到期计划并预占名额，同一时刻按 ID 决定顺序。
func (s *Service) dispatch(now time.Time) time.Time {
	var due []*entry
	var next time.Time
	earliest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, e := range s.alarms {
		if e.removed || e.snap.Next.IsZero() {
			continue
		}
		if e.snap.Next.After(now) {
			earliest(e.snap.Next)
		} else {
			due = append(due, e)
		}
	}
	slices.SortFunc(due, func(a, b *entry) int {
		return cmp.Or(a.snap.Next.Compare(b.snap.Next), cmp.Compare(a.spec.ID, b.spec.ID))
	})
	for _, e := range due {
		when := e.snap.Next
		reason := "schedule"
		if e.startup {
			reason, e.startup = "startup", false
		}
		s.planRegular(e, now)
		earliest(e.snap.Next)
		s.admit(e, reason, when)
	}
	return next
}
