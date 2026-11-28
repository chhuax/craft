package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/app"
)

// 提交路径只置一位。事件内容、顺序和跨进程可见性全部以库内游标为准，
// 因此连续提交可以合并唤醒，即使 AfterCommit 并发倒序也不会漏事件。
type eventHub struct {
	wake    chan struct{}
	mu      sync.Mutex
	clients map[chan struct{}]context.CancelFunc
}

func newEventHub() *eventHub {
	return &eventHub{wake: make(chan struct{}, 1), clients: map[chan struct{}]context.CancelFunc{}}
}
func (h *eventHub) notify() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *eventHub) subscribe(cancel context.CancelFunc) (chan struct{}, func()) {
	ch := make(chan struct{}, 8)
	h.mu.Lock()
	h.clients[ch] = cancel
	h.mu.Unlock()
	return ch, func() { h.mu.Lock(); delete(h.clients, ch); h.mu.Unlock() }
}

func (h *eventHub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, cancel := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
			delete(h.clients, ch)
			cancel()
		}
	}
}

func (h *eventHub) run(ctx context.Context) {
	// 另一进程的引擎命令不会调用本进程的 AfterCommit，同一读路径也负责定时补拉。
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		case <-tick.C:
		}
		h.broadcast()
	}
}

func (a *localAPI) stream(w http.ResponseWriter, r *http.Request) {
	since := int64(0)
	if values, ok := r.URL.Query()["since"]; ok {
		var err error
		since, err = strconv.ParseInt(values[0], 10, 64)
		if err != nil || since < 0 || len(values) != 1 {
			http.Error(w, "游标无效", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// 先登记再查库，避免读历史到切换实时通知的空窗。
	wake, remove := a.events.subscribe(func() {
		cancel()
		// 取消 context 不能唤醒已经卡在 socket 上的 Write。底层连接支持并发 Close。
		if conn, ok := r.Context().Value(streamConnKey{}).(net.Conn); ok {
			_ = conn.Close()
		}
	})
	defer remove()
	w.Header().Set("Content-Type", "text/event-stream")
	c := http.NewResponseController(w)
	if err := c.Flush(); err != nil {
		return
	}
	for ctx.Err() == nil {
		var page eventBatch
		err := a.daemon.View(ctx, func(reader app.CommandReader) error {
			var err error
			page, err = eventPage(reader, since, pageSize)
			return err
		})
		if err != nil {
			return
		}
		if len(page.Items) > 0 {
			if err := c.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return
			}
			for _, e := range page.Items {
				raw, err := json.Marshal(e)
				if err != nil || ctx.Err() != nil || e.Rev != since+1 {
					return
				}
				if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.Rev, raw); err != nil {
					return
				}
				since = e.Rev
			}
			if err := c.Flush(); err != nil {
				return
			}
		}
		if page.NextSince != 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-wake:
		}
	}
}
