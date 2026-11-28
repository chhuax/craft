package method

import (
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

// ConfirmedDoc 记录被确认的相对路径和内容摘要。主文档另记当时档位，供重新定档时核对。
type ConfirmedDoc struct {
	Path string `json:"path"`
	Hash string `json:"hash"`
	Tier string `json:"tier,omitempty"`
}

// Confirmation 是人的一次决定。时间由调用方提供，同一变更只需保留当前使用的确认。
type Confirmation struct {
	Version int            `json:"version"`
	By      string         `json:"by"`
	At      time.Time      `json:"at"`
	Docs    []ConfirmedDoc `json:"docs"`
}

// Confirm 根据人实际看过的各份文档生成确认。文档结构上的缺口由 CheckExit 单独报告。
func Confirm(by string, at time.Time, docs ...Document) (*Confirmation, error) {
	c := Confirmation{Version: formatVersion, By: strings.TrimSpace(by), At: at.UTC()}
	for _, d := range docs {
		if d.unsupported || d.hash == "" {
			return nil, fmt.Errorf("确认文档 %q：版本不支持或文档未读入", d.Path)
		}
		tier, _ := d.value("tier").(string)
		if !validTier(tier) {
			tier = ""
		}
		c.Docs = append(c.Docs, ConfirmedDoc{d.Path, d.hash, tier})
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c Confirmation) validate() error {
	if c.Version != formatVersion {
		return fmt.Errorf("确认记录的版本 %d 不受支持", c.Version)
	}
	if strings.TrimSpace(c.By) == "" || c.At.IsZero() || len(c.Docs) == 0 {
		return fmt.Errorf("确认记录缺少确认人、时间或文档")
	}
	seen := make(map[string]bool)
	for _, d := range c.Docs {
		if !safePath(d.Path) || seen[d.Path] {
			return fmt.Errorf("确认记录的文档路径无效或重复：%q", d.Path)
		}
		seen[d.Path] = true
		raw, err := hex.DecodeString(d.Hash)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("确认文档 %q 的内容摘要无效", d.Path)
		}
		if d.Tier != "" && !validTier(d.Tier) {
			return fmt.Errorf("确认文档 %q 的档位无效：%q", d.Path, d.Tier)
		}
	}
	return nil
}

// DocStatus 是单份文档的核对结果，移动时保留新位置。
type DocStatus struct {
	Path, State, MovedTo string
}

// Verification 汇总已确认文档的当前状态：none、confirmed 或 stale。
type Verification struct {
	State string
	Docs  []DocStatus
}

// Verify 只使用调用方提供的当前文档和移动候选。原路径存在时优先核对，防止副本掩盖正文改动。
// 原路径缺失时按同名、同摘要核对；多个相同副本取调用方给定顺序中的第一个。
func Verify(c *Confirmation, docs ...Document) Verification {
	if c == nil {
		return Verification{State: "none"}
	}
	if err := c.validate(); err != nil {
		return Verification{State: "stale"}
	}
	byPath := make(map[string]Document, len(docs))
	byName := make(map[string][]Document)
	for _, doc := range docs {
		if !safePath(doc.Path) {
			continue
		}
		if _, duplicate := byPath[doc.Path]; duplicate {
			return Verification{State: "stale"}
		}
		byPath[doc.Path] = doc
		name := path.Base(doc.Path)
		byName[name] = append(byName[name], doc)
	}
	result := Verification{State: "confirmed"}
	for _, saved := range c.Docs {
		status := DocStatus{Path: saved.Path, State: "missing"}
		if d, ok := byPath[saved.Path]; ok {
			status.State = "changed"
			if saved.matches(d) {
				status.State = "ok"
			}
		} else {
			for _, d := range byName[path.Base(saved.Path)] {
				if saved.matches(d) {
					status.State = "moved"
					status.MovedTo = d.Path
					break
				}
			}
		}
		if status.State == "missing" || status.State == "changed" {
			result.State = "stale"
		}
		result.Docs = append(result.Docs, status)
	}
	return result
}

func (saved ConfirmedDoc) matches(d Document) bool {
	return !d.unsupported && saved.Hash == d.hash && (saved.Tier == "" || d.value("tier") == saved.Tier)
}
