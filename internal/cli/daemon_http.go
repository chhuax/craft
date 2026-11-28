package cli

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/pkg/protocol"
)

const maxRPCBody = 64 << 10
const connectionLimit = 128

type localAPI struct {
	state  daemonState
	home   string
	daemon *app.Daemon
	events *eventHub
	stop   context.CancelFunc
	force  func()
}

func allowedHost(got, ip, localhost string) bool { return got == ip || got == localhost }
func authorized(header, token string) bool {
	value, ok := strings.CutPrefix(header, "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(value), []byte(token)) == 1
}

func (a *localAPI) handler() http.Handler {
	ip := a.state.address()
	local := "localhost:" + strconv.Itoa(a.state.Port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if _, hasOrigin := r.Header["Origin"]; hasOrigin || r.URL.IsAbs() || !allowedHost(r.Host, ip, local) {
			http.Error(w, "请求来源被拒绝", http.StatusForbidden)
			return
		}
		if len(r.Header["Authorization"]) != 1 || !authorized(r.Header.Get("Authorization"), a.state.Token) {
			http.Error(w, "需要本地令牌", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/events":
			if r.Method != "GET" {
				http.Error(w, "需要 GET", http.StatusMethodNotAllowed)
				return
			}
			a.stream(w, r)
		case "/rpc":
			if r.Method != "POST" {
				http.Error(w, "需要 POST", http.StatusMethodNotAllowed)
				return
			}
			typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if len(r.Header["Content-Type"]) != 1 || err != nil || typ != "application/json" {
				http.Error(w, "需要 application/json", http.StatusUnsupportedMediaType)
				return
			}
			a.rpc(w, r)
		default:
			http.NotFound(w, r)
		}
	})
}

func (a *localAPI) rpc(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRPCBody))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "请求体过大", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "读取请求失败", http.StatusBadRequest)
		}
		return
	}
	var req protocol.Request
	reply := protocol.Response{JSONRPC: "2.0", ID: json.RawMessage("null")}
	switch {
	case !json.Valid(raw):
		reply.Error = rpcError(-32700, "JSON 无效")
	case jsonv2.Unmarshal(raw, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" || !validRPCID(req.ID):
		reply.Error = rpcError(-32600, "请求无效")
	default:
		reply.ID = req.ID
		result, err := a.dispatch(r.Context(), req)
		if err == nil {
			reply.Result, err = jsonv2.Marshal(result)
		}
		if err != nil {
			reply.Error = publicRPCError(err)
		}
	}
	size := len(reply.Result) + len(reply.ID)
	if reply.Error != nil && reply.Error.Data != nil {
		size += len(*reply.Error.Data)
	}
	if size > maxResponse-128 {
		reply.Result = nil
		reply.Error = rpcError(-32603, "结果超过单页上限")
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(reply); err != nil {
		slog.Debug("本地 RPC 响应中断", "error", err)
	}
}

func validRPCID(id json.RawMessage) bool {
	return len(id) > 0 && (id[0] == '"' || id[0] == '-' || id[0] >= '0' && id[0] <= '9')
}
func publicRPCError(err error) *protocol.Error {
	if e, ok := errors.AsType[*protocol.Error](err); ok {
		return e
	}
	switch {
	case errors.Is(err, app.ErrUnknownExtension), errors.Is(err, app.ErrUnknownAlarm), errors.Is(err, os.ErrNotExist):
		return rpcError(-32004, "找不到请求的对象")
	case errors.Is(err, app.ErrAlarmUnavailable):
		return rpcError(-32006, "扩展未就绪")
	default:
		slog.Error("本地 RPC 失败", "error", err)
		return rpcError(-32603, "本地操作失败")
	}
}
func rpcError(code int, message string) *protocol.Error {
	return &protocol.Error{Code: code, Message: message}
}

type rpcParams struct {
	SinceRev        *int64 `json:"sinceRev"`
	Limit           *int   `json:"limit"`
	AttentionCursor string `json:"attentionCursor"`
	WorkCursor      string `json:"workCursor"`
	Ext             string `json:"ext"`
	Alarm           string `json:"alarm"`
	ID              string `json:"id"`
	Tail            *int   `json:"tail"`
	Force           bool   `json:"force"`
}

func (a *localAPI) dispatch(ctx context.Context, req protocol.Request) (any, error) {
	switch req.Method {
	case "act", "outbox.list", "outbox.show", "outbox.confirm", "outbox.withdraw":
		return a.action(ctx, req)
	}
	var p rpcParams
	bad := rpcError(-32602, "参数无效")
	if req.Params != nil && (!bytes.HasPrefix(*req.Params, []byte("{")) || jsonv2.Unmarshal(*req.Params, &p) != nil) {
		return nil, bad
	}
	switch req.Method {
	case "status":
		return map[string]any{"running": true, "pid": a.state.PID, "version": a.state.Version, "startedAt": a.state.StartedAt, "address": a.state.address(), "fingerprintStale": a.daemon.Stale(), "health": a.daemon.Health()}, nil
	case "daemon.stop":
		if p.Force {
			a.force()
		} else {
			a.stop()
		}
		return map[string]bool{"stopping": true}, nil
	case "snapshot", "events":
		return a.read(ctx, req.Method, p)
	case "alarm.list":
		return map[string]any{"items": a.daemon.Alarms()}, nil
	case "alarm.run":
		if p.Ext == "" || p.Alarm == "" || strings.Contains(p.Ext+p.Alarm, ":") {
			return nil, bad
		}
		id, err := a.daemon.QueueAlarm(ctx, p.Ext, p.Alarm)
		return map[string]any{"requestId": id, "queued": true}, err
	case "ext.enable", "ext.disable":
		if p.ID == "" {
			return nil, bad
		}
		enabled := req.Method == "ext.enable"
		err := a.daemon.SetExtensionEnabled(ctx, p.ID, enabled)

		return map[string]any{"id": p.ID, "enabled": enabled, "effective": "nextStart"}, err
	case "ext.list":
		h := a.daemon.Health()
		return map[string]any{"running": true, "extensions": h.Extensions, "rejected": h.Rejected}, nil
	case "ext.logs":
		return logPage(a.home, p.ID, p.Tail)
	default:
		return nil, rpcError(-32601, "方法不存在")
	}
}

func (a *localAPI) read(ctx context.Context, method string, p rpcParams) (any, error) {
	since := int64(0)
	if p.SinceRev != nil {
		since = *p.SinceRev
	}
	limit := pageSize
	if p.Limit != nil {
		limit = *p.Limit
	}
	if since < 0 || limit < 1 || limit > 100 {
		return nil, rpcError(-32602, "分页参数无效")
	}
	var result any
	err := a.daemon.View(ctx, func(r app.CommandReader) error {
		if method == "events" {
			page, err := eventPage(r, since, limit)
			result = page
			return err
		}
		rev, err := r.Revision()
		if err != nil {
			return err
		}
		if p.SinceRev != nil && since == rev {
			result = map[string]any{"rev": rev, "unchanged": true}
			return nil
		}
		items, err := r.Attention()
		if err != nil {
			return err
		}
		counts, err := r.Counts(len(items))
		if err != nil {
			return err
		}
		attention, err := attentionPage(items, p.AttentionCursor)
		if err != nil {
			return rpcError(-32602, "待办游标已失效")
		}
		works, err := workPage(r, true, "", p.WorkCursor)
		if err != nil {
			return err
		}
		result = map[string]any{"rev": rev, "counts": counts, "attention": attention.payload, "openWorks": works.payload}
		return nil
	})
	return result, err
}

func logPage(home, id string, tail *int) (any, error) {
	lines := 50
	if tail != nil {
		lines = *tail
	}
	values, err := exthost.Tail(filepath.Join(home, "logs"), id, lines)
	if errors.Is(err, exthost.ErrInvalidParams) {
		return nil, rpcError(-32602, "扩展 ID 或行数无效")
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "lines": values}, nil
}

// 限制已接受的 TCP 连接，覆盖尚未发完请求头的连接。
type cappedListener struct {
	net.Listener
	slots chan struct{}
}

func (l cappedListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &countedConn{Conn: c, release: func() { <-l.slots }}, nil
		default:
			_ = c.Close()
		}
	}
}

type countedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *countedConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }

type streamConnKey struct{}

func (a *localAPI) server(ctx context.Context) *http.Server {
	return &http.Server{
		DisableGeneralOptionsHandler: true,
		Handler:                      a.handler(),
		BaseContext:                  func(net.Listener) context.Context { return ctx },
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, streamConnKey{}, c)
		},
		ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}
}
