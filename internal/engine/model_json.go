package engine

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
)

// 聚合在新建时常有 nil 切片。即使调用方用 encoding/json 的旧入口，也要写出数组或对象。
func marshalAggregate(out *jsontext.Encoder, value any) error {
	return jsonv2.MarshalEncode(out, value, jsonv2.FormatNilSliceAsNull(false), jsonv2.FormatNilMapAsNull(false))
}

// MarshalJSONTo 保证 Work 必需的空集合仍有约定的 JSON 类型。
func (v Work) MarshalJSONTo(out *jsontext.Encoder) error {
	type wire Work
	return marshalAggregate(out, (*wire)(&v))
}

// MarshalJSONTo 保证 Thread 必需的空集合仍有约定的 JSON 类型。
func (v Thread) MarshalJSONTo(out *jsontext.Encoder) error {
	type wire Thread
	return marshalAggregate(out, (*wire)(&v))
}

// MarshalJSONTo 保证 Trigger 必需的空集合仍有约定的 JSON 类型。
func (v Trigger) MarshalJSONTo(out *jsontext.Encoder) error {
	type wire Trigger
	return marshalAggregate(out, (*wire)(&v))
}

// MarshalJSONTo 保证 TriggerDisposition 必需的空集合仍有约定的 JSON 类型。
func (v TriggerDisposition) MarshalJSONTo(out *jsontext.Encoder) error {
	type wire TriggerDisposition
	return marshalAggregate(out, (*wire)(&v))
}

// MarshalJSONTo 保证 Evaluation 必需的空集合仍有约定的 JSON 类型。
func (v Evaluation) MarshalJSONTo(out *jsontext.Encoder) error {
	type wire Evaluation
	return marshalAggregate(out, (*wire)(&v))
}

// MarshalJSONTo 保证 Decision 必需的空集合仍有约定的 JSON 类型。
func (v Decision) MarshalJSONTo(out *jsontext.Encoder) error {
	type wire Decision
	return marshalAggregate(out, (*wire)(&v))
}
