package exthost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"

	"github.com/chhuax/craft/pkg/protocol"
)

type unavailableError struct{}

// Error 给没有使用 errors.Is 的调用方提供断连说明。
func (unavailableError) Error() string { return "扩展连接不可用" }

// Retryable 允许上层在重新握手后按自己的策略重试。
func (unavailableError) Retryable() bool { return true }

// ErrUnavailable 表示连接已断开，可用 errors.Is 判断，调用方可在重连后重试。
var ErrUnavailable = unavailableError{}

// ErrMethodNotFound 由 Handler 返回，映射到协议错误 -32601。
var ErrMethodNotFound = errors.New("未知方法")

// ErrInvalidParams 由 Handler 包装具体参数错误，映射到 -32602。
var ErrInvalidParams = errors.New("参数无效")

// Handler 接收入站请求与通知，须响应 ctx 取消，并在返回前结束对该请求的处理。
// 它可对向 Call；Close 等待处理完成，不能在回调内同步等待 Close。
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   *protocol.Error `json:"error"`
}

// 内核生成的请求 id 从 1 递增，零值专用于省略通知的 id。
type outboundRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      uint64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type incoming struct {
	msg    envelope
	ctx    context.Context
	cancel context.CancelFunc
	key    string
}

// Conn 独占读写两端，支持双向并发调用；断开连接也会取消已派发的 Handler。
type Conn struct {
	r         io.Reader
	w         io.Writer
	handler   Handler
	limit     int
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
	mu        sync.Mutex
	err       error
	sequence  uint64
	pending   map[uint64]chan envelope
	active    map[string]context.CancelFunc
	queue     []incoming
	workers   int
	output    [][]byte
	wake      chan struct{}
}

// NewConn 即刻开始读消息，maxLine 为零时使用 16 MiB。对可能阻塞的传输端须提供
// 可中止对应 Read/Write 的 io.Closer，使 Close 能收回后台 goroutine。
func NewConn(r io.Reader, w io.Writer, handler Handler, maxLine int) *Conn {
	if maxLine <= 0 {
		maxLine = 16 << 20
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		r: r, w: w, handler: handler, limit: maxLine,
		ctx: ctx, cancel: cancel,
		pending: map[uint64]chan envelope{},
		active:  map[string]context.CancelFunc{},
		wake:    make(chan struct{}, 1),
	}
	c.wg.Go(c.readLoop)
	c.wg.Go(c.writeLoop)
	return c
}

// Close 可以并发重复调用。先中止传输并通知所有调用，再等待读写和 Handler 退出。
func (c *Conn) Close() error {
	c.fail(io.ErrClosedPipe)
	c.wg.Wait()
	return c.closeErr
}

func (c *Conn) fail(reason error) {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.err = fmt.Errorf("%w: %w", ErrUnavailable, reason)
		c.cancel()
		c.queue = nil
		c.output = nil
		c.active = nil
		c.pending = nil
		c.mu.Unlock()
		// 先唤醒调用者，再在锁外中止两个方向可能阻塞的 IO。
		for _, end := range []any{c.r, c.w} {
			if closer, ok := end.(io.Closer); ok {
				if err := closer.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					c.closeErr = errors.Join(c.closeErr, err)
				}
			}
		}
	})
}

func (c *Conn) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) send(msg any) error {
	// RawMessage 中的字符串 id 必须按来包拼写回送，包括原有的转义。
	data, err := jsonv2.Marshal(msg, json.DefaultOptionsV1(), jsontext.EscapeForHTML(false), jsontext.EscapeForJS(false), jsontext.PreserveRawStrings(true))
	if err != nil {
		return fmt.Errorf("编码 RPC: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return c.err
	}
	c.output = append(c.output, append(data, '\n'))
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return nil
}

func (c *Conn) writeLoop() {
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
		}
		c.mu.Lock()
		batch := c.output
		c.output = nil
		c.mu.Unlock()
		for i, b := range batch {
			n, err := c.w.Write(b)
			if err == nil && n != len(b) {
				err = io.ErrShortWrite
			}
			if err != nil {
				c.fail(err)
				return
			}
			batch[i] = nil
		}
	}
}

// Call 发起请求并将成功响应解码到 result；传 nil 可只等待完成。超时由 ctx 控制。
func (c *Conn) Call(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, false)
}

// CallWaiting 在取消后仍等待远端响应或断连，供需要保留运行名额的调用使用。
func (c *Conn) CallWaiting(ctx context.Context, method string, params, result any) error {
	return c.call(ctx, method, params, result, true)
}

func (c *Conn) call(ctx context.Context, method string, params, result any, wait bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.sequence++
	id := c.sequence
	reply := make(chan envelope, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	err := c.send(outboundRequest{protocol.JSONRPCVersion, id, method, params})
	if err != nil {
		return err
	}
	var msg envelope
	select {
	case <-ctx.Done():
		// 取消通知排在原请求之后。闹钟还占用远端名额，只有响应或断连才能释放。
		_ = c.send(outboundRequest{JSONRPC: protocol.JSONRPCVersion, Method: "$/cancel", Params: struct {
			ID uint64 `json:"id"`
		}{id}})
		if !wait {
			return ctx.Err()
		}
		select {
		case <-reply:
			return ctx.Err()
		case <-c.ctx.Done():
			return c.failure()
		}
	case msg = <-reply:
	case <-c.ctx.Done():
		// 对端可能在写完最后一个响应后立即退出，已送达的响应优先于 EOF。
		select {
		case msg = <-reply:
		default:
			return c.failure()
		}
	}
	if msg.Error != nil {
		return msg.Error
	}
	if raw, ok := result.(*json.RawMessage); ok && raw != nil {
		*raw = msg.Result
		return nil
	}
	if result != nil {
		if err := json.Unmarshal(msg.Result, result); err != nil {
			return fmt.Errorf("%s result: %w", method, err)
		}
	}
	return nil
}

func (c *Conn) readLoop() {
	reader := bufio.NewReader(c.r)
	var scratch []byte
	for {
		if _, err := reader.Peek(1); err != nil {
			c.fail(err)
			return
		}
		// 只取已经到达的字节。即使超限后对端不再写入，也能在换行之前断开。
		chunk, _ := reader.Peek(reader.Buffered())
		end := bytes.IndexByte(chunk, '\n')
		consumed := len(chunk)
		if end >= 0 {
			chunk = chunk[:end]
			consumed = end + 1
		}
		if len(chunk) > c.limit-len(scratch) {
			c.fail(fmt.Errorf("RPC 行超过 %d 字节", c.limit))
			return
		}
		switch {
		case end < 0:
			scratch = append(scratch, chunk...)
		case len(scratch) > 0:
			scratch = append(scratch, chunk...)
			c.receive(scratch)
			scratch = scratch[:0]
		default:
			c.receive(chunk) // 短行直接引用 bufio 缓冲，RawMessage 已经独立拥有其内容。
		}
		_, _ = reader.Discard(consumed)
	}
}

func (c *Conn) receive(line []byte) {
	var msg envelope
	if err := jsonv2.Unmarshal(line, &msg); err != nil {
		code := -32600
		if _, ok := errors.AsType[*jsontext.SyntacticError](err); ok || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			code = -32700
		}
		c.rpcError(nil, code, err.Error())
		return
	}
	if msg.JSONRPC != protocol.JSONRPCVersion {
		c.rpcError(nil, -32600, "jsonrpc 版本无效")
		return
	}
	if msg.Method != "" {
		c.request(msg)
		return
	}
	if msg.Result == nil && msg.Error == nil {
		c.rpcError(nil, -32600, "缺少 method 或响应内容")
		return
	}
	if msg.Error != nil && msg.Result != nil {
		c.fail(errors.New("RPC 响应同时含 result 和 error"))
		return
	}
	id, err := strconv.ParseUint(string(msg.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	reply := c.pending[id]
	delete(c.pending, id)
	if reply != nil {
		reply <- msg
	}
	c.mu.Unlock()
}

func (c *Conn) rpcError(id json.RawMessage, code int, message string) {
	_ = c.send(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: id, Error: &protocol.Error{Code: code, Message: message}})
}

func idKey(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	switch b := raw[0]; {
	case b == '"':
		// 同一字符串的转义拼写可以不同，回应时仍使用来包的原始 id。
		if bytes.IndexByte(raw, '\\') >= 0 {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				return `"` + s
			}
		}
		return string(raw[:len(raw)-1])
	case b == '-' || b >= '0' && b <= '9':
		return string(raw)
	default:
		return ""
	}
}

func (c *Conn) request(msg envelope) {
	key := idKey(msg.ID)
	if msg.ID != nil && key == "" {
		c.rpcError(nil, -32600, "id 须为数字或字符串")
		return
	}
	if msg.Params != nil && msg.Params[0] != '{' && msg.Params[0] != '[' {
		if msg.ID != nil {
			c.rpcError(msg.ID, -32602, "params 须为对象或数组")
		}
		return
	}
	if msg.Method == "$/cancel" && msg.ID == nil {
		var p struct {
			ID json.RawMessage `json:"id"`
		}
		if json.Unmarshal(msg.Params, &p) == nil {
			c.mu.Lock()
			cancel := c.active[idKey(p.ID)]
			c.mu.Unlock()
			if cancel != nil {
				cancel()
			}
		}
		return
	}
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	if key != "" && c.active[key] != nil {
		c.mu.Unlock()
		c.rpcError(msg.ID, -32600, "重复的请求 id")
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	job := incoming{msg, ctx, cancel, key}
	if key != "" {
		c.active[key] = cancel
	}
	// 读循环不能等槽：已占槽的 Handler 可能正在等待同一连接上的对向响应。
	if c.workers >= 64 {
		c.queue = append(c.queue, job)
	} else {
		c.workers++
		c.wg.Go(func() { c.work(job) })
	}
	c.mu.Unlock()
}

func (c *Conn) work(job incoming) {
	for {
		c.handle(job)
		c.mu.Lock()
		if len(c.queue) == 0 {
			c.workers--
			c.mu.Unlock()
			return
		}
		job = c.queue[0]
		c.queue[0] = incoming{}
		c.queue = c.queue[1:]
		if len(c.queue) == 0 {
			c.queue = nil
		}
		c.mu.Unlock()
	}
}

func (c *Conn) handle(job incoming) {
	var value any
	err := job.ctx.Err()
	if err == nil {
		if c.handler == nil {
			err = ErrMethodNotFound
		} else {
			value, err = c.handler(job.ctx, job.msg.Method, job.msg.Params)
		}
	}
	// 响应写出后对端即可复用同一个 id，必须先从可取消集合摘除旧请求。
	c.mu.Lock()
	delete(c.active, job.key)
	c.mu.Unlock()
	job.cancel()
	if job.msg.ID == nil {
		return
	}
	if err == nil {
		err = c.send(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  any             `json:"result"`
		}{protocol.JSONRPCVersion, job.msg.ID, value})
	}
	if err != nil {
		if remote, ok := errors.AsType[*protocol.Error](err); ok {
			err = c.send(protocol.Response{JSONRPC: protocol.JSONRPCVersion, ID: job.msg.ID, Error: remote})
			if err == nil {
				return
			}
			// Handler 返回的错误详情也可能含无效 JSON，仍须给对端一个可解码的错误。

		}
		code := -32603
		switch {
		case errors.Is(err, ErrMethodNotFound):
			code = -32601
		case errors.Is(err, ErrInvalidParams):
			code = -32602
		}
		c.rpcError(job.msg.ID, code, err.Error())
	}
}
