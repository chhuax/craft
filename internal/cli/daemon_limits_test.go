package cli

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestSlowSubscriber(t *testing.T) {
	hub := newEventHub()
	slow, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, remove := hub.subscribe(cancel)
	defer remove()
	fast, removeFast := hub.subscribe(func() { t.Error("及时读取的订阅者被断开") })
	defer removeFast()
	for range cap(ch) + 1 {
		hub.notify()
		<-hub.wake
		hub.broadcast()
		<-fast
	}
	if slow.Err() == nil {
		t.Fatal("慢订阅者仍持有连接")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.clients) != 1 {
		t.Fatal("订阅没有收回")
	}
	// 即使分发器被暂停，提交也只做一次非阻塞置位。
	for range 1000 {
		hub.notify()
	}
}

func TestRPCDoesNotExposePaths(t *testing.T) {
	a := localAPI{home: t.TempDir(), state: daemonState{Port: 42, Token: "secret"}}
	r := httptest.NewRequest("POST", "/rpc", strings.NewReader(`{"jsonrpc":"2.0","method":"ext.logs","params":{"id":"missing"},"id":1}`))
	r.Host = "127.0.0.1:42"
	r.Header.Set("Authorization", "Bearer secret")
	r.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.handler().ServeHTTP(rec, r)
	w := rec.Body.String()
	if strings.Contains(w, a.home) || strings.Contains(w, "stack") || !strings.Contains(w, "-32004") {
		t.Fatal(w)
	}
}

type pipeListener struct {
	accepts chan net.Conn
	done    chan struct{}
	once    sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{accepts: make(chan net.Conn), done: make(chan struct{})}
}
func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.accepts:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestConnectionCap(t *testing.T) {
	l := newPipeListener()
	limited := cappedListener{l, make(chan struct{}, 1)}
	first, peer := net.Pipe()
	go func() { l.accepts <- first }()
	c, err := limited.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	next := make(chan net.Conn)
	go func() { accepted, _ := limited.Accept(); next <- accepted }()
	second, other := net.Pipe()
	l.accepts <- second
	if _, err := other.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	_ = other.Close()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	third, peer3 := net.Pipe()
	l.accepts <- third
	if err := (<-next).Close(); err != nil {
		t.Fatal(err)
	}
	_ = peer3.Close()
}

func TestHTTPDeadlines(t *testing.T) {
	for _, part := range []string{"header", "body", "write"} {
		t.Run(part, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				api := localAPI{state: daemonState{Port: 1234, Token: "x"}}
				srv := api.server(t.Context())
				l := newPipeListener()
				ended := make(chan error, 1)
				go func() { ended <- srv.Serve(l) }()
				client, server := net.Pipe()
				l.accepts <- server
				request := "POST /rpc HTTP/1.1\r\n"
				if part != "header" {
					request += "Host: localhost:1234\r\nAuthorization: Bearer x\r\nContent-Type: application/json\r\nContent-Length: 1\r\n\r\n"
				}
				if part == "write" {
					request += "{"
				}
				if _, err := io.WriteString(client, request); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				// 虚拟时间只在请求已进入读或写等待后推进。
				time.Sleep(2 * (srv.ReadTimeout + srv.WriteTimeout))
				if _, err := io.ReadAll(client); err != nil {
					t.Fatal(err)
				}
				_ = client.Close()
				if err := srv.Close(); err != nil {
					t.Fatal(err)
				}
				if err := <-ended; !errors.Is(err, http.ErrServerClosed) {
					t.Fatal(err)
				}
			})
		})
	}
}

func TestLogsRejectEscape(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "private.log"), []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	command(t, home, 2, "ext", "logs", "../private")
}

func TestGuardAllocs(t *testing.T) {
	token := strings.Repeat("a", 32)
	header := "Bearer " + token
	if n := testing.AllocsPerRun(1000, func() {
		if !authorized(header, token) || !allowedHost("localhost:42", "127.0.0.1:42", "localhost:42") {
			t.Fatal("合法请求被拒绝")
		}
	}); n != 0 {
		t.Fatalf("%v allocs", n)
	}
}

type observedWrite struct {
	net.Conn
	started chan struct{}
	once    sync.Once
}

func (c *observedWrite) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestBlockedStreamDisconnected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hub := newEventHub()
		api := localAPI{state: daemonState{Port: 1234, Token: "x"}, events: hub}
		server := api.server(t.Context())
		l := newPipeListener()
		done := make(chan error, 1)
		go func() { done <- server.Serve(l) }()
		client, peer := net.Pipe()
		defer func() {
			if err := errors.Join(client.Close(), server.Close()); err != nil {
				t.Error(err)
			}
			if err := <-done; !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		}()
		c := &observedWrite{Conn: peer, started: make(chan struct{})}
		l.accepts <- c
		if _, err := io.WriteString(client, "GET /events HTTP/1.1\r\nHost: localhost:1234\r\nAuthorization: Bearer x\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		<-c.started
		// 对端连响应头都没读；溢出时必须立即打断挂起的写操作。
		for range 9 {
			hub.broadcast()
		}
		synctest.Wait()
		var p [1]byte
		if n, err := client.Read(p[:]); n != 0 || !errors.Is(err, io.EOF) {
			t.Fatalf("慢连接没有断开: %d %v", n, err)
		}

	})
}

func TestStopEscalation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopped := false
		process := exitProbe{exited: func() (bool, error) { return stopped, nil }}
		if err := waitForStop(t.Context(), process, nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
		start := time.Now()
		err := waitForStop(t.Context(), process, func() error {
			if time.Since(start) < 15*time.Second {
				t.Fatal("未等待优雅停止")
			}
			stopped = true
			return io.EOF
		})
		if err != nil || !stopped {
			t.Fatal(err)
		}
		if err := waitForStop(t.Context(), process, func() error { t.Fatal("已退出仍收到强制停止"); return nil }); err != nil {
			t.Fatal(err)
		}
	})
}
