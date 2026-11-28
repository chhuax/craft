package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/engine"
)

func BenchmarkLocalRPC(b *testing.B) {
	for _, threads := range []int{100, 10000} {
		b.Run(fmt.Sprintf("threads_%d", threads), func(b *testing.B) {
			home := b.TempDir()
			seedWatch(b, home, threads)
			dir := filepath.Join(home, "extensions")
			if err := os.Mkdir(dir, 0700); err != nil {
				b.Fatal(err)
			}
			d := app.NewDaemon(app.DaemonConfig{StateDir: home, ExtensionDirs: []string{dir}, Version: "dev"})
			if err := d.Start(b.Context()); err != nil {
				b.Fatal(err)
			}
			defer func() {
				if err := d.Stop(context.Background()); err != nil {
					b.Error(err)
				}
			}()
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			st := daemonState{PID: os.Getpid(), Version: "dev", StartedAt: time.Now().UTC(), Port: ln.Addr().(*net.TCPAddr).Port, Token: rand.Text()}
			api := localAPI{state: st, home: home, daemon: d}
			srv := api.server(b.Context())
			done := make(chan error, 1)
			go func() { done <- srv.Serve(cappedListener{ln, make(chan struct{}, connectionLimit)}) }()
			defer func() {
				if err := srv.Close(); err != nil {
					b.Error(err)
				}
				if err := <-done; !errors.Is(err, http.ErrServerClosed) {
					b.Error(err)
				}
			}()
			for _, method := range []string{"status", "snapshot"} {
				b.Run(method, func(b *testing.B) {
					b.ReportAllocs()
					for b.Loop() {
						if _, err := st.call(b.Context(), method, nil); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func BenchmarkCommitSubscribers(b *testing.B) {
	for _, n := range []int{0, 100} {
		b.Run(fmt.Sprintf("subscribers_%d", n), func(b *testing.B) {
			repo := app.NewSQLiteStore(testDB(b, b.TempDir()))
			hub := newEventHub()
			var clients []chan struct{}
			for range n {
				ch, _ := hub.subscribe(func() { b.Error("订阅者丢失") })
				clients = append(clients, ch)
			}
			// 实际写入、分配 rev、提交并执行 AfterCommit。利用唤醒确认分发完成，
			// 在计时区间外排空每个客户端，使全部样本始终保有 N 个订阅者。
			served := make(chan struct{})
			finish := make(chan struct{})
			go func() {
				defer close(finish)
				for range hub.wake {
					hub.broadcast()
					served <- struct{}{}
				}
			}()
			defer func() { close(hub.wake); <-finish }()
			var elapsed time.Duration
			for b.Loop() {
				start := time.Now()
				err := repo.Atomic(b.Context(), func(u engine.Unit) error {
					if n > 0 {
						u.AfterCommit(hub.notify)
					}
					return u.Emit(engine.Event{At: testNow, Type: "thread.described"})
				})
				elapsed += time.Since(start)
				if err != nil {
					b.Fatal(err)
				}
				if n > 0 {
					<-served
					for _, ch := range clients {
						<-ch
					}
				}
			}
			b.ReportMetric(float64(elapsed.Nanoseconds())/float64(b.N), "ns/commit")
		})
	}
}

func BenchmarkDaemonReady(b *testing.B) {
	b.Setenv("CRAFT_EXTENSIONS", "")
	b.Setenv("GORACE", "atexit_sleep_ms=0")
	var samples []time.Duration
	for b.Loop() {
		b.StopTimer()
		b.Setenv("CRAFT_HOME", b.TempDir())
		b.StartTimer()
		start := time.Now()
		daemonCommand(b, "daemon", "start")
		samples = append(samples, time.Since(start))
		b.StopTimer()
		daemonCommand(b, "daemon", "stop")
		b.StartTimer()
	}
	slices.Sort(samples)
	b.ReportMetric(float64(samples[len(samples)/2].Microseconds())/1000, "ms/median")
}

func BenchmarkRPCGuards(b *testing.B) {
	token := rand.Text()
	header := "Bearer " + token
	ip, local := "127.0.0.1:52317", "localhost:52317"
	b.ReportAllocs()
	for b.Loop() {
		if !authorized(header, token) || !allowedHost(ip, ip, local) {
			b.Fatal("令牌或 Host 未通过")
		}
	}
}
