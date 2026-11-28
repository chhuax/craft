package method

import (
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ClosingRun 记录调用方实测的命令、退出码和前后快照；未跑完时 Exit 为 nil。
type ClosingRun struct {
	Command string `json:"cmd"`
	Exit    *int   `json:"exit"`
	Before  string `json:"before"`
	After   string `json:"after"`
}

// ScenarioVerdict 保存验证者对一个场景的结论和可定位的依据。
type ScenarioVerdict struct {
	ID       string `json:"id"`
	Result   string `json:"result"`
	Evidence string `json:"evidence"`
}

// VerifierReport 是验证者交回的原始报告。只有全部场景都有证据且均通过，整体才可通过。
type VerifierReport struct {
	Version   int               `json:"version"`
	By        string            `json:"by"`
	Scenarios []ScenarioVerdict `json:"scenarios"`
}

// PendingAssumption 来自需求原文，交给人核对，验证者不能把它记作通过。
type PendingAssumption struct {
	ID   string `json:"id,omitempty"`
	Text string `json:"text"`
}

// IndependentReview 引用日志中实际读过的收尾记录（行号从 1 起），防止并发收尾串用报告。
type IndependentReview struct {
	ClosingLine int                 `json:"closingLine"`
	Report      VerifierReport      `json:"report"`
	Pending     []PendingAssumption `json:"pending"`
}

// Record 是状态目录里的一条事实。三种内容互斥，先后顺序取日志行序。
type Record struct {
	Version int                `json:"version"`
	At      time.Time          `json:"at"`
	Closing *ClosingRun        `json:"closing,omitempty"`
	Review  *IndependentReview `json:"review,omitempty"`
	Fact    string             `json:"fact,omitempty"`
}

func validDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func (r Record) validate() error {
	if r.Version != formatVersion || r.At.IsZero() {
		return fmt.Errorf("事实记录版本或时间无效")
	}
	switch {
	case r.Closing != nil && r.Review == nil && r.Fact == "":
		c := r.Closing
		if strings.TrimSpace(c.Command) == "" || !validDigest(c.Before) || c.After != "" && !validDigest(c.After) {
			return fmt.Errorf("收尾命令或快照无效")
		}
	case r.Review != nil && r.Closing == nil && r.Fact == "":
		if r.Review.ClosingLine < 1 {
			return fmt.Errorf("独立报告缺少收尾记录")
		}
		return r.Review.Report.validate()
	case strings.TrimSpace(r.Fact) != "" && r.Closing == nil && r.Review == nil:
	default:
		return fmt.Errorf("每条日志需包含一种事实")
	}
	return nil
}

func (r VerifierReport) validate() error {
	if r.Version != formatVersion || strings.TrimSpace(r.By) == "" || len(r.Scenarios) == 0 {
		return fmt.Errorf("独立报告缺少支持的版本、验证者或逐项结论")
	}
	seen := map[string]bool{}
	for _, s := range r.Scenarios {
		if seen[s.ID] || !scenarioID.MatchString(s.ID) {
			return fmt.Errorf("报告场景 %q 重复或无效", s.ID)
		}
		seen[s.ID] = true
		switch s.Result {
		case "pass", "fail", "unverified":
		default:
			return fmt.Errorf("场景 %s 的结论 %q 无效", s.ID, s.Result)
		}
		if strings.TrimSpace(s.Evidence) == "" {
			return fmt.Errorf("场景 %s 缺少证据位置", s.ID)
		}
	}
	return nil
}

func (r VerifierReport) check(req Document) error {
	if err := r.validate(); err != nil {
		return err
	}
	if req.unsupported || len(r.Scenarios) != len(req.Scenarios) {
		return fmt.Errorf("报告未逐项对应需求中的场景")
	}
	ids := make(map[string]bool, len(r.Scenarios))
	for _, s := range r.Scenarios {
		ids[s.ID] = true
	}
	for _, s := range req.Scenarios {
		if !ids[s.ID] {
			return fmt.Errorf("需求场景 %s 重复或缺少结论", s.ID)
		}
		delete(ids, s.ID)
	}
	return nil
}

func pendingAssumptions(req Document) []PendingAssumption {
	var pending []PendingAssumption
	for _, a := range req.Assumptions {
		pending = append(pending, PendingAssumption{a.ID, a.Text})
	}
	return pending
}

func (r VerifierReport) result() string {
	for _, s := range r.Scenarios {
		if s.Result != "pass" {
			return "fail"
		}
	}
	return "pass"
}

// EvidenceStatus 给出收尾与独立验证各自的 none、stale、fail、unverified 或 pass 结论。
type EvidenceStatus struct{ Closing, Review string }

func latestEvidence(log []Record) (closing, review int) {
	closing, review = -1, -1
	for i, r := range log {
		if r.Closing != nil {
			closing, review = i, -1
		}
		if r.Review != nil && closing >= 0 && r.Review.ClosingLine == closing+1 {
			review = i
		}
	}
	return closing, review
}

func (c ClosingRun) state(current string) string {
	if c.Exit == nil || !validDigest(c.After) || c.Before != c.After {
		return "unverified"
	}
	if c.After != current {
		return "stale"
	}
	if *c.Exit != 0 {
		return "fail"
	}
	return "pass"
}

// AssessEvidence 只用给定的文档、事实和当前快照判断有效性。新收尾记录会替换上一轮依据。
func AssessEvidence(req Document, log []Record, current string) EvidenceStatus {
	s := EvidenceStatus{"none", "none"}
	full, review := latestEvidence(log)
	if full < 0 {
		return s
	}
	if err := log[full].validate(); err != nil {
		s.Closing = "unverified"
		return s
	}
	s.Closing = log[full].Closing.state(current)
	if review < 0 {
		return s
	}
	r := log[review].Review
	if s.Closing != "pass" || r.Report.check(req) != nil || !slices.Equal(r.Pending, pendingAssumptions(req)) || log[review].validate() != nil {
		s.Review = "stale"
	} else {
		s.Review = r.Report.result()
	}
	return s
}
