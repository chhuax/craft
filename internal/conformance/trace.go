package conformance

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sync"

	"github.com/chhuax/craft/pkg/protocol"
)

type event struct {
	Method     string          `json:"method"`
	ID         json.RawMessage `json:"id"`
	Params     json.RawMessage `json:"params"`
	Result     json.RawMessage `json:"result"`
	Error      *protocol.Error `json:"error"`
	in         bool
	number     int
	stage      string
	badJSON    bool
	httpStatus int
	request    string
	query      url.Values
	empty      bool
	detail     string
}

func (e event) where() string {
	name := e.Method
	if e.httpStatus != 0 {
		name = e.request
	}
	return fmt.Sprintf("%s 第 %d 条 %s", e.stage, e.number, name)
}

// 读、写和 HTTP 各自来自独立 goroutine；在读循环派发前、写循环完成发送时记录，
// 才不会把 Handler 启动或返回的调度顺序误作协议顺序。
type transcript struct {
	mu       sync.Mutex
	stage    string
	events   []event
	onCancel func()
}

func (t *transcript) begin(stage string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stage = stage
	return len(t.events)
}

func (t *transcript) record(e event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e.number = len(t.events) + 1
	e.stage = t.stage
	t.events = append(t.events, e)
	if !e.in && e.Method == "$/cancel" && t.onCancel != nil {
		t.onCancel()
		t.onCancel = nil
	}
}

func (t *transcript) wire(in bool, b []byte) {
	var e event
	if !jsontext.Value(b).IsValid() {
		e.badJSON = true
		e.detail = fmt.Sprintf("期望严格 UTF-8 JSON，收到 %.120q", b)
	} else {
		// 合法 JSON 但形状错误由 Conn 返回协议错误；它仍然属于此处的严格 JSON 样本。
		_ = jsonv2.Unmarshal(b, &e)
	}
	e.in = in
	t.record(e)
}

func (t *transcript) http(q *http.Request, status int, empty bool, detail string) {
	t.record(event{httpStatus: status, query: q.URL.Query(), request: q.Method + " " + q.URL.RequestURI(), empty: empty, detail: detail})
}

func (t *transcript) since(start int) []event {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.events[start:])
}
