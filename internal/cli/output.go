package cli

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

func clipText(s string) string {
	const limit = 600
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

// 外部字段可以是任意 JSON。短值保留原类型，长值给出完整 UTF-8 前缀。
func preview(raw json.RawMessage) any {
	if len(raw) <= 1024 {
		return raw
	}
	end := 1024
	for !utf8.RuneStart(raw[end]) {
		end--
	}
	return map[string]any{"preview": string(raw[:end]), "truncated": true}
}

func render(w io.Writer, r result, asJSON bool) error {
	if r.evidence == nil {
		r.evidence = []any{}
	}
	// 单行标识和扩展自定义字段也可能极长。写 stdout 之前核对整页，避免输出半个信封。
	raw, err := jsonv2.Marshal(r.payload, jsonv2.Deterministic(true))
	if err != nil {
		return err
	}
	evidence, err := jsonv2.Marshal(r.evidence, jsonv2.Deterministic(true))
	if err != nil {
		return err
	}
	if len(raw)+len(evidence) > 2<<20 {
		return fmt.Errorf("单页输出超过 2 MiB")
	}
	if asJSON {
		_, err = fmt.Fprintf(w, "{\"ok\":true,\"payload\":%s,\"evidence\":%s}\n", raw, evidence)
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(fields)) {
		v := fields[k]
		if k == "version" && len(fields) == 1 || k == "help" {
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return err
			}
			_, err := fmt.Fprintln(w, s)
			return err
		}
		if err := writeField(w, k, v); err != nil {
			return err
		}
	}
	for _, ev := range r.evidence {
		raw, err := jsonv2.Marshal(ev, jsonv2.Deterministic(true))
		if err != nil {
			return err
		}
		if err := writeField(w, "evidence", raw); err != nil {
			return err
		}
	}
	return nil
}

func writeField(w io.Writer, key string, value json.RawMessage) error {
	if bytes.HasPrefix(value, []byte("[")) {
		var items []json.RawMessage
		if err := json.Unmarshal(value, &items); err != nil {
			return err
		}
		if len(items) > 0 {
			for _, item := range items {
				if err := writeRow(w, key, item); err != nil {
					return err
				}
			}
			return nil
		}
	}
	_, err := fmt.Fprintf(w, "%s\t%s\n", key, strings.TrimSpace(string(value)))
	return err
}

func writeRow(w io.Writer, name string, raw json.RawMessage) error {
	if len(raw) == 0 || raw[0] != '{' {
		_, err := fmt.Fprintf(w, "%s\t%s\n", name, raw)
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	var line strings.Builder
	line.WriteString(name)
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		fmt.Fprintf(&line, "\t%s=%s", key, fields[key])
	}
	_, err := fmt.Fprintln(w, line.String())
	return err
}
