package protocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestOmitOptionalNull(t *testing.T) {
	for _, tc := range []struct {
		name, wire, field string
		target            any
	}{
		{"请求参数", `{"jsonrpc":"2.0","id":1,"method":"shutdown","params":null}`, "params", &Request{}},
		{"通知参数", `{"jsonrpc":"2.0","method":"ready","params":null}`, "params", &Notification{}},
		{"错误详情", `{"code":-32601,"message":"方法不存在","data":null}`, "data", &Error{}},
		{"显示名", `{"source":"demo","id":"a","name":null}`, "name", &ActorRef{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.wire), tc.target); err != nil {
				t.Fatal(err)
			}
			wire, err := json.Marshal(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatal(err)
			}
			if _, ok := fields[tc.field]; ok {
				t.Errorf("可选字段 %s 为 null 时应省略：%s", tc.field, wire)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   string
		target any
	}{
		{"事件", `{"id":"e1","source":"demo","event":"item.changed","threads":[{"source":"demo","kind":"item","id":"41"}],"actor":{"source":"demo","id":"a","name":"林"},"human":true,"addressed":"mention","at":"2025-01-01T00:00:00.000Z","text":"请看","channel":{"source":"demo","thread":"41","room":{"id":9007199254740995}},"url":"https://example.org/41","meta":{"n":9007199254740993}}`, &Trigger{}},
		{"可空与可选字段", `{"id":"e2","source":"demo","event":"item.closed","threads":[{"source":"demo","kind":"item","id":"41"}],"actor":{"source":"demo","id":"a"},"human":false,"addressed":null,"at":"2025-01-01T00:00:00.000Z","text":"","meta":{}}`, &Trigger{}},
		{"结构化地址", `{"source":"demo","thread":{"kind":"conversation","id":"42"},"messageId":"3"}`, &ChannelRef{}},
		{"数字请求标识", `{"jsonrpc":"2.0","id":9007199254740993,"method":"initialize","params":{"protocol":1}}`, &Request{}},
		{"字符串请求标识", `{"jsonrpc":"2.0","id":"x","method":"shutdown"}`, &Request{}},
		{"通知", `{"jsonrpc":"2.0","method":"$/cancel","params":{"id":"x"}}`, &Notification{}},
		{"无参通知", `{"jsonrpc":"2.0","method":"shutdown"}`, &Notification{}},
		{"成功响应", `{"jsonrpc":"2.0","id":"x","result":{"ok":true}}`, &Response{}},
		{"空结果", `{"jsonrpc":"2.0","id":9007199254740993,"result":null}`, &Response{}},
		{"错误响应", `{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"解析错误","data":{"offset":1}}}`, &Response{}},
		{"无详情错误", `{"code":-32601,"message":"方法不存在"}`, &Error{}},
		{"握手", `{"protocol":1,"kernelVersion":"v1","config":{},"stateDir":"state"}`, &InitializeParams{}},
		{"能力", `{"ok":true,"capabilities":{"future":true}}`, &InitializeResult{}},
		{"闹钟调用", `{"alarmId":"daily","reason":"startup","scheduledAtMs":123}`, &AlarmRunParams{}},
		{"执行动作", `{"action":"send","params":{"n":9007199254740993},"preview":false,"idempotencyKey":"abc"}`, &ActionExecuteParams{}},
		{"核实动作", `{"action":"send","idempotencyKey":"abc"}`, &ActionVerifyParams{}},
		{"读取上下文", `{"thread":{"source":"demo","kind":"item","id":"a"},"trigger":"e","resource":{"id":1},"cursor":"next","messageId":"m"}`, &ContextReadParams{}},
		{"上下文可选字段", `{"thread":{"source":"demo","kind":"item","id":"a"}}`, &ContextReadParams{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(tc.wire), tc.target); err != nil {
				t.Fatal(err)
			}
			output, err := json.Marshal(tc.target)
			if err != nil {
				t.Fatal(err)
			}
			var got, want any
			decoder := json.NewDecoder(bytes.NewReader(output))
			decoder.UseNumber()
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			decoder = json.NewDecoder(strings.NewReader(tc.wire))
			decoder.UseNumber()
			if err := decoder.Decode(&want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("往返后得到 %s；期望 %s", output, tc.wire)
			}
		})
	}
}
