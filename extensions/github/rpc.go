package main

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *protocol.Error `json:"error,omitempty"`
}

type peer struct {
	out     io.Writer
	writeMu sync.Mutex
	mu      sync.Mutex
	seq     uint64
	pending map[string]chan frame
	active  map[string]context.CancelFunc
}

func (p *peer) send(f frame) error {
	f.JSONRPC = protocol.JSONRPCVersion
	wire, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("编码 RPC: %w", err)
	}
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	_, err = p.out.Write(append(wire, '\n'))
	return err
}

func (p *peer) call(ctx context.Context, method string, params, dst any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("编码 %s: %w", method, err)
	}
	ch := make(chan frame, 1)
	p.mu.Lock()
	p.seq++
	id := strconv.FormatUint(p.seq, 10)
	p.pending[id] = ch
	p.mu.Unlock()
	defer func() { p.mu.Lock(); delete(p.pending, id); p.mu.Unlock() }()
	if err = p.send(frame{ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case reply := <-ch:
		if reply.Error != nil {
			return fmt.Errorf("内核拒绝 %s（%d）", method, reply.Error.Code)
		}
		if dst != nil {
			if err := json.Unmarshal(reply.Result, dst); err != nil {
				return fmt.Errorf("解码 %s 回执失败", method)
			}
		}
		return nil
	}
}

func invalid(message string) error { return &protocol.Error{Code: -32602, Message: message} }

func rpcError(err error) *protocol.Error {
	if e, ok := errors.AsType[*protocol.Error](err); ok {
		return e
	}
	if errors.Is(err, context.Canceled) {
		return &protocol.Error{Code: -32800, Message: "请求已取消"}
	}
	return &protocol.Error{Code: -32603, Message: err.Error()}
}

func (p *peer) respond(id json.RawMessage, result any, err error) error {
	f := frame{ID: id}
	if err == nil {
		f.Result, err = json.Marshal(result)
	}
	if err != nil {
		f.Error = rpcError(err)
	}
	return p.send(f)
}

func (p *peer) route(f frame) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if f.Method == "" {
		if ch := p.pending[string(f.ID)]; ch != nil {
			delete(p.pending, string(f.ID))
			ch <- f
		}
		return true
	}
	if f.Method == "$/cancel" {
		var x struct{ ID json.RawMessage }
		if json.Unmarshal(f.Params, &x) == nil {
			if cancel := p.active[string(x.ID)]; cancel != nil {
				cancel()
			}
		}
		return true
	}
	return f.ID == nil
}

func serve(in io.ReadCloser, out io.Writer, now func() time.Time) error {
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	p := &peer{out: out, pending: make(map[string]chan frame), active: make(map[string]context.CancelFunc)}
	var ext *extension
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	for scanner.Scan() {
		var f frame
		if !jsontext.Value(scanner.Bytes()).IsValid() || json.Unmarshal(scanner.Bytes(), &f) != nil || f.JSONRPC != "2.0" {
			return errors.New("无效的 JSON-RPC 消息")
		}
		if p.route(f) {
			continue
		}
		switch f.Method {
		case "shutdown":
			cancel()
			workers.Wait()
			return p.respond(f.ID, nil, nil)
		case "initialize":
			var err error
			if ext != nil {
				err = invalid("已经完成握手")
			} else {
				ext, err = initialize(f.Params, p.call, now)
			}
			if err = p.respond(f.ID, protocol.InitializeResult{OK: err == nil, Capabilities: json.RawMessage(`{}`)}, err); err != nil {
				return err
			}
			continue
		}
		if ext == nil {
			if err := p.respond(f.ID, nil, invalid("请先握手")); err != nil {
				return err
			}
			continue
		}
		// 先登记再派发，保证紧随请求的取消通知能够找到工作协程。
		work, stop := context.WithCancel(ctx)
		id := string(f.ID)
		p.mu.Lock()
		p.active[id] = stop
		p.mu.Unlock()
		workers.Go(func() {
			defer stop()
			v, err := ext.handle(work, f.Method, f.Params)
			p.mu.Lock()
			delete(p.active, id)
			p.mu.Unlock()
			if p.respond(f.ID, v, err) != nil {
				cancel()
				_ = in.Close()
			}
		})
	}
	return scanner.Err()
}

func main() {
	if err := serve(os.Stdin, os.Stdout, time.Now); err != nil {
		// 线上的请求与响应都可能含私有文本，stderr 只记录固定诊断。
		fmt.Fprintln(os.Stderr, "GitHub 扩展连接已中断")
		os.Exit(1)
	}
}
