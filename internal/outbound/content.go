package outbound

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
)

// ContentRules 中任一规则命中即可转入确认；规则可能被多个请求并发调用。
type ContentRules []func(string) bool

// Matches 对已经解码的文本进行检查，以便直接测试或复用同一张规则表。
func (rules ContentRules) Matches(s string) bool {
	for _, match := range rules {
		if match(s) {
			return true
		}
	}
	return false
}

var (
	localPath  = regexp.MustCompile(`(?:^|[^[:alnum:]_:/.-])(?:/(?:Users|Volumes|home|root|private|tmp|etc|var|opt)/|~/|[a-zA-Z]:[\\/]|\\\\[[:alnum:]_-]+\\)[^[:space:]"']+`)
	credential = regexp.MustCompile(`(?i)\b(?:password|passwd|secret|token|api[_-]?key|authorization|cookie|session[_-]?id)["']?\s*[:=]\s*["']?[^[:space:]"',}\]]+|\bBearer\s+[A-Za-z0-9._~+/=-]{8,}|-----BEGIN (?:[A-Z0-9]+ )?PRIVATE KEY-----|\beyJ[\w-]+\.[\w-]+\.[\w-]+`)
	stackFrame = regexp.MustCompile(`(?m)Traceback \(most recent call last\)|^goroutine \d+ \[[^]\n]+\]:|^\s+at [\w.$<>]+\([^()\n]*:\d+\)|^\s*File "[^"\n]+", line \d+`)
)

// DefaultContentRules 提供预编译的中立规则，调用方可替换或追加规则。
func DefaultContentRules() ContentRules {
	return ContentRules{
		func(s string) bool { return strings.ContainsAny(s, `/\`) && localPath.MatchString(s) },
		credential.MatchString,
		func(s string) bool { return strings.ContainsAny(s, `(["`) && stackFrame.MatchString(s) },
		internalAddress,
	}
}

func internalAddress(s string) bool {
	for token := range strings.FieldsFuncSeq(s, func(r rune) bool {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune(".:-_", r)
		return !valid
	}) {
		// 单个冒号既可跟端口，也可在正文里分隔标签和主机名。
		if strings.Count(token, ":") == 1 {
			left, right, _ := strings.Cut(token, ":")
			if privateHost(left) || privateHost(right) {
				return true
			}
		} else if privateHost(token) {
			return true
		}
	}
	return false
}

func privateHost(host string) bool {
	host = strings.TrimRight(host, ".")
	if strings.EqualFold(host, "localhost") || len(host) > 6 && strings.EqualFold(host[len(host)-6:], ".local") {
		return true
	}
	if !strings.ContainsAny(host, ".:") {
		return false
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && (ip.Unmap().IsPrivate() || ip.Unmap().IsLoopback())
}

func (rules ContentRules) checkJSON(raw json.RawMessage) (bool, error) {
	if len(raw) == 0 {
		return false, nil
	}
	// 整体检查覆盖凭据键值对，再读字符串以识别转义路径和多行堆栈。
	wire := jsontext.Value(raw)
	if bytes.ContainsRune(raw, '\\') {
		// 将转义的字段名与相邻值一起交给键值规则，如此才能识别完整的凭据形态。
		wire = wire.Clone()
		if err := wire.Format(); err != nil {
			return false, fmt.Errorf("解码待检查的字段: %w", err)
		}
	}
	if rules.Matches(string(wire)) {
		return true, nil
	}
	d := jsontext.NewDecoder(bytes.NewReader(raw))
	for {
		tok, err := d.ReadToken()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("读取待检查的 JSON: %w", err)
		}
		if tok.Kind() == '"' && rules.Matches(tok.String()) {
			return true, nil
		}
	}
}
