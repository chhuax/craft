package engine

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
)

type refFields struct {
	Source string `json:"source"`
	Kind   string `json:"kind"`
	ID     string `json:"id"`
}

// ThreadRef 同时保存扩展标识与确定性键。分量只读，避免缓存键与对象身份分离。
type ThreadRef struct {
	fields refFields
	key    string
}

// NewThreadRef 在对象进入领域时分配一次键，后续索引和跨命令传递共享只读字符串。
func NewThreadRef(source, kind, id string) ThreadRef {
	return ThreadRef{fields: refFields{source, kind, id}, key: source + ":" + kind + ":" + id}
}

// Source 返回声明对象的来源。
func (r ThreadRef) Source() string { return r.fields.Source }

// Kind 返回扩展声明的对象类型。
func (r ThreadRef) Kind() string { return r.fields.Kind }

// ID 返回来源内的对象标识。
func (r ThreadRef) ID() string { return r.fields.ID }

// ThreadKey 返回可直接用于索引的键，即使调用方长期保留结果也无需再分配。
func ThreadKey(ref ThreadRef) string {
	if ref.key == "" {
		return "::"
	}
	return ref.key
}

// MarshalJSONTo 只持久化三个分量，键总能重建。
func (r ThreadRef) MarshalJSONTo(out *jsontext.Encoder) error {
	return jsonv2.MarshalEncode(out, &r.fields)
}

// UnmarshalJSONFrom 替换整个引用，确保换对象时也换键。
func (r *ThreadRef) UnmarshalJSONFrom(in *jsontext.Decoder) error {
	var f refFields
	if err := jsonv2.UnmarshalDecode(in, &f); err != nil {
		return err
	}
	*r = NewThreadRef(f.Source, f.Kind, f.ID)
	return nil
}

// ActorRef 以来源和标识确定发送人，显示名可变。
type ActorRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`
}

// ChannelRef 保留完整回复地址，路由字段及其余扩展字段均保持原始 JSON 精度。
type ChannelRef struct {
	Source string                     `json:"source"`
	Thread json.RawMessage            `json:"thread"`
	Extra  map[string]json.RawMessage `json:",embed"`
}
