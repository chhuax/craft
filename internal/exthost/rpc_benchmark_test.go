package exthost

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
	"testing"

	"github.com/chhuax/craft/pkg/protocol"
)

// 每个调用者同时只有一个在途请求，实际并发数不受 GOMAXPROCS 或 RunParallel 的倍数影响。
func measureRPC(b *testing.B, n int, call func(int) error) {
	b.Helper()
	b.ReportAllocs()
	var wg sync.WaitGroup
	b.ResetTimer()
	for i := range n {
		wg.Go(func() {
			for j := i; j < b.N; j += n {
				if err := call(i); err != nil {
					b.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "calls/s")
}

func BenchmarkCall(b *testing.B) {
	for _, n := range []int{1, 8, 64} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			requests, tx := io.Pipe()
			rx, responses := io.Pipe()
			c := NewConn(rx, tx, nil, 0)
			var wg sync.WaitGroup
			wg.Go(func() {
				defer func() { _ = responses.Close() }()
				rd := bufio.NewReader(requests)
				wr := json.NewEncoder(responses)
				for {
					line, err := rd.ReadSlice('\n')
					if err != nil {
						return
					}
					var req protocol.Request
					if err = json.Unmarshal(line, &req); err != nil {
						b.Error(err)
						return
					}
					if err = wr.Encode(protocol.Response{JSONRPC: "2.0", ID: req.ID, Result: *req.Params}); err != nil {
						b.Error(err)
						return
					}
				}
			})
			args := json.RawMessage(`{"n":41}`)
			measureRPC(b, n, func(_ int) error {
				var out struct{ N int }
				err := c.Call(b.Context(), "echo", args, &out)
				if err == nil && out.N != 41 {
					return fmt.Errorf("响应 %d", out.N)
				}
				return err
			})
			_ = c.Close()
			wg.Wait()
			_ = requests.Close()
		})
	}
}

func BenchmarkHandler(b *testing.B) {
	for _, n := range []int{1, 8, 64} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			rx, requests := io.Pipe()
			responses, tx := io.Pipe()
			c := NewConn(rx, tx, func(_ context.Context, _ string, p json.RawMessage) (any, error) { return p, nil }, 0)
			replies := make([]chan struct{}, n)
			encoders := make([]*json.Encoder, n)
			calls := make([]protocol.Request, n)
			args := json.RawMessage(`{"n":41}`)
			for i := range n {
				replies[i] = make(chan struct{}, 1)
				encoders[i] = json.NewEncoder(requests)
				calls[i] = protocol.Request{JSONRPC: "2.0", Method: "echo", ID: json.RawMessage(strconv.Itoa(i)), Params: &args}
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				rd := bufio.NewReader(responses)
				for {
					line, err := rd.ReadSlice('\n')
					if err != nil {
						return
					}
					var res struct {
						ID     int
						Result struct{ N int }
					}
					if err = json.Unmarshal(line, &res); err != nil || res.Result.N != 41 {
						b.Errorf("响应 %s: %v", line, err)
						return
					}
					replies[res.ID] <- struct{}{}
				}
			}()
			measureRPC(b, n, func(i int) error {
				if err := encoders[i].Encode(calls[i]); err != nil {
					return err
				}
				select {
				case <-replies[i]:
					return nil
				case <-done:
					return io.ErrUnexpectedEOF
				}
			})
			_ = c.Close()
			<-done
			_ = responses.Close()
			_ = requests.Close()
		})
	}
}
