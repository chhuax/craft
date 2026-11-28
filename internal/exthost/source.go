package exthost

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"time"

	"github.com/chhuax/craft/pkg/protocol"
)

// SourceScope 从连接绑定的 manifest 复制授权目录，可供多个入站请求并发读取。
type SourceScope struct {
	events map[[2]string]bool
	kinds  map[[2]string]ThreadKind
}

// NewSourceScope 只接受已装载的声明，调用参数不能覆盖这里的来源归属。
func NewSourceScope(m Manifest) SourceScope {
	s := SourceScope{map[[2]string]bool{}, map[[2]string]ThreadKind{}}
	for _, src := range m.WatchSources {
		for _, ev := range src.Events {
			s.events[[2]string{src.ID, ev.Event}] = true
		}
		for _, k := range src.ThreadKinds {
			s.kinds[[2]string{src.ID, k.ID}] = k
		}
	}
	return s
}

// Kind 返回本连接有权操作的对象类型及其稳定标识约定。
func (s SourceScope) Kind(source, kind string) (ThreadKind, bool) {
	k, ok := s.kinds[[2]string{source, kind}]
	return k, ok
}

func invalid(detail string) error { return fmt.Errorf("%w: %s", ErrInvalidParams, detail) }

func (s SourceScope) authorize(ref protocol.ThreadRef) error {
	if _, ok := s.Kind(ref.Source, ref.Kind); !ok || ref.ID == "" {
		return invalid("对象不属于本扩展声明的来源与类型")
	}
	return nil
}

func (s SourceScope) fromKey(key string) (protocol.ThreadRef, error) {
	src, tail, _ := strings.Cut(key, ":")
	kind, id, _ := strings.Cut(tail, ":")
	ref := protocol.ThreadRef{Source: src, Kind: kind, ID: id}
	return ref, s.authorize(ref)
}

// time.Parse 会进位越界的偏移分量；协议与引擎只接受能表示为四位 UTC 年份的时间。
var sourceTime = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}([.,][0-9]+)?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func isoTime(at string) (string, error) {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil || !sourceTime.MatchString(at) || t.UTC().Year() < 0 || t.UTC().Year() > 9999 {
		return "", invalid(fmt.Sprintf("时间格式或时区无效 %q", at))
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z"), nil
}

// Normalize 检查 Trigger 的权限与必填项，按协议规范时间并截断长文本，不修改入参。
func (s SourceScope) Normalize(p protocol.Trigger) (protocol.Trigger, error) {
	switch {
	case p.ID == "":
		return p, invalid("缺少事件 ID")
	case p.Source == ReservedSource || !s.events[[2]string{p.Source, p.Event}]:
		return p, invalid(fmt.Sprintf("未授权的来源或事件 %q/%q", p.Source, p.Event))
	case len(p.Threads) == 0:
		return p, invalid("事件缺少主对象")
	case p.Addressed != nil && *p.Addressed != "direct" && *p.Addressed != "mention":
		return p, invalid("addressed 无效")
	}
	for _, ref := range p.Threads {
		if err := s.authorize(ref); err != nil {
			return p, err
		}
	}
	at, err := isoTime(p.At)
	if err != nil {
		return p, err
	}
	p.At = at
	n := 0
	for offset := range p.Text {
		if n == 4000 {
			p.Text = p.Text[:offset]
			p.Meta = maps.Clone(p.Meta)
			if p.Meta == nil {
				p.Meta = map[string]json.RawMessage{}
			}
			p.Meta["truncated"] = json.RawMessage("true")
			break
		}
		n++
	}
	return p, nil
}

// Fact 保留扩展自定义事实值的精度和形状。
type Fact struct {
	Value   json.RawMessage  `json:"value"`
	Label   string           `json:"label,omitempty"`
	Problem *json.RawMessage `json:"problem,omitempty"`
}

// Describe 指定合并的元信息。Decode 会把 key 与 ref 统一为已授权的 Ref。
type Describe struct {
	Key   *string             `json:"key"`
	Ref   *protocol.ThreadRef `json:"ref"`
	Label *string             `json:"label"`
	URL   *string             `json:"url"`
	Facts map[string]*Fact    `json:"facts"`
}

// Patch 携带按键合并的对象元信息。
type Patch struct {
	Key  string                     `json:"key"`
	Meta map[string]json.RawMessage `json:"meta"`
}

// Reopened 标识已核实状态的对象及时间。
type Reopened struct {
	Thread protocol.ThreadRef `json:"thread"`
	At     string             `json:"at"`
}

// Closed 为关闭事实增加结果和说明。
type Closed struct {
	Reopened
	Outcome string `json:"outcome"`
	Detail  string `json:"detail"`
}

// Bind 将本扩展声明的对象绑定至指定工作。
type Bind struct {
	WorkID string             `json:"workId"`
	Thread protocol.ThreadRef `json:"thread"`
	Own    *bool              `json:"own"`
}

// HealthReport 描述扩展自己的健康条目。
type HealthReport struct {
	Entry  string `json:"entry"`
	State  string `json:"state"`
	Detail string `json:"detail"`
}

// GetThread 只允许读取本连接声明的对象键。
type GetThread struct {
	Key string `json:"key"`
}

// Subscribed 可按来源内的对象类型筛选订阅。
type Subscribed struct {
	Kind string `json:"kind"`
}

// RecentReceipts 限定扩展按提交顺序读取的回执页。
type RecentReceipts struct {
	After int64 `json:"afterSentSeq"`
	Limit int   `json:"limit"`
}

// GetState 按连接所属扩展读取私有键。
type GetState struct {
	Key string `json:"key"`
}

// SetState 以原始 JSON 保存私有状态。
type SetState struct {
	GetState
	Value json.RawMessage `json:"value"`
}

// MaxStateKeys 限制单个扩展持有的私有键数，更新已有键不占用新名额。
const MaxStateKeys = 128
const maxStateValue = 4 << 10

// Decode 按方法解码并授权入站请求；只解释该方法的已知字段。
func (s SourceScope) Decode(method string, raw json.RawMessage) (any, error) {
	var p any
	switch method {
	case "watch/receive":
		p = new(protocol.Trigger)
	case "thread/describe":
		p = new(Describe)
	case "thread/patch":
		p = new(Patch)
	case "thread/closed":
		p = new(Closed)
	case "thread/reopened":
		p = new(Reopened)
	case "work/bindThread":
		p = new(Bind)
	case "health/report":
		p = new(HealthReport)
	case "thread/get":
		p = new(GetThread)
	case "thread/subscribed":
		p = new(Subscribed)
	case "receipts/recent":
		p = new(RecentReceipts)
	case "state/get":
		p = new(GetState)
	case "state/set":
		p = new(SetState)
	case "state/keys", "work/open":
		p = new(struct{})
	default:
		return nil, ErrMethodNotFound
	}
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if raw[0] != '{' {
		return nil, invalid("参数须为对象")
	}
	if err := jsonv2.Unmarshal(raw, p); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidParams, err)
	}
	return p, s.validateCall(p)
}

func (s SourceScope) validateCall(arg any) error {
	switch p := arg.(type) {
	case *protocol.Trigger:
		normalized, err := s.Normalize(*p)
		*p = normalized
		return err
	case *Describe:
		if (p.Key == nil) == (p.Ref == nil) {
			return invalid("须指定 key、ref 之一")
		}
		if p.Key != nil {
			ref, err := s.fromKey(*p.Key)
			if err != nil {
				return err
			}
			p.Ref = &ref
		}
		for _, fact := range p.Facts {
			if fact != nil && fact.Value == nil {
				return invalid("fact 缺少 value")
			}
		}
		return s.authorize(*p.Ref)
	case *Patch:
		if _, err := s.fromKey(p.Key); err != nil {
			return err
		}
		if p.Meta == nil {
			return invalid("缺少 meta")
		}
		for k, v := range p.Meta {
			switch k {
			case "facts":
				return invalid("facts 须经 describe 更新")
			case "label", "url":
				if v[0] != '"' && string(v) != "null" {
					return invalid("meta 字段须为字符串或 null")
				}
			}
		}
	case *GetThread:
		_, err := s.fromKey(p.Key)
		return err
	case *Closed:
		if p.Outcome != "done" && p.Outcome != "closed" {
			return invalid("outcome 无效")
		}
		return s.validateCall(&p.Reopened)
	case *Reopened:
		if err := s.authorize(p.Thread); err != nil {
			return err
		}
		at, err := isoTime(p.At)
		p.At = at
		return err
	case *Bind:
		if p.WorkID == "" || p.Own == nil {
			return invalid("缺少 workId 或 own")
		}
		return s.authorize(p.Thread)
	case *HealthReport:
		if p.Entry == "" || p.State != "ok" && p.State != "paused" && p.State != "error" {
			return invalid("健康条目或状态无效")
		}
	case *RecentReceipts:
		if p.After < 0 || p.Limit <= 0 {
			return invalid("回执游标或条数无效")
		}
	case *SetState:
		if len(p.Value) == 0 || len(p.Value) > maxStateValue {
			return invalid("私有状态值的大小无效")
		}
		return s.validateCall(&p.GetState)
	case *GetState:
		if p.Key == "" || len(p.Key) > 256 {
			return invalid("私有状态键的大小无效")
		}
	}
	return nil
}
