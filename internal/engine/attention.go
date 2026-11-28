package engine

import (
	"cmp"
	"slices"
	"strings"
)

// AttentionSet 由调用方加载活跃对象和相关决策。决策可乱序，须包含能取代旧失败的后续结论。
type AttentionSet struct {
	Works     []Work
	Threads   []Thread
	Decisions []Decision
}

// AttentionRef 只携带定位该项所需的标识，换问题或换决策会生成新的待办标识。
type AttentionRef struct {
	Work      string `json:"work,omitempty"`
	Attention string `json:"attention,omitempty"`
	Decision  string `json:"decision,omitempty"`
	Thread    string `json:"thread,omitempty"`
	Trigger   string `json:"trigger,omitempty"`
}

// AttentionItem 是活跃事实的派生结果，可直接序列化供展示或操作。
type AttentionItem struct {
	ID       string       `json:"id"`
	Kind     string       `json:"kind"`
	Severity string       `json:"severity"`
	Ref      AttentionRef `json:"ref"`
	Title    string       `json:"title"`
	Detail   string       `json:"detail"`
	Actions  []string     `json:"actions"`
	At       string       `json:"at"`
}

// Attention 从已加载的状态计算待办，按操作优先、时间倒序、确定性标识升序返回。
func Attention(set AttentionSet) []AttentionItem {
	capacity := len(set.Works) + min(len(set.Threads), len(set.Decisions))
	items := make([]AttentionItem, 0, capacity)
	for i := range set.Works {
		if item, ok := workAttention(&set.Works[i]); ok {
			items = append(items, item)
		}
	}
	byThread := make(map[string][]*Decision, min(len(set.Threads), len(set.Decisions)))
	for i := range set.Decisions {
		d := &set.Decisions[i]
		byThread[d.ThreadKey] = append(byThread[d.ThreadKey], d)
	}
	for i := range set.Threads {
		th := &set.Threads[i]
		if item, ok := threadWarning(th, byThread[th.Key]); ok {
			items = append(items, item)
		}
	}
	slices.SortFunc(items, func(a, b AttentionItem) int {
		return cmp.Or(strings.Compare(a.Severity, b.Severity), strings.Compare(b.At, a.At), strings.Compare(a.ID, b.ID))
	})
	return items
}

func workAttention(w *Work) (AttentionItem, bool) {
	stage := WorkStage(*w)
	if stage.Stage != "person" {
		return AttentionItem{}, false
	}
	item := AttentionItem{Kind: stage.Action, Severity: "action", Ref: AttentionRef{Work: w.ID}, Title: clip(w.Title), At: w.UpdatedAt}
	var report *Report
	if n := len(w.Assignments); n > 0 {
		report = w.Assignments[n-1].Report
	}
	if report != nil {
		item.At = cmp.Or(report.At, w.UpdatedAt)
		item.Detail = clip(report.Summary)
	}
	switch stage.Action {
	case "answer":
		q := w.Attention
		item.Ref.Attention = q.ID
		item.Detail = clip(q.Question)
		item.At = cmp.Or(q.CreatedAt, item.At)
		item.Actions = []string{"answer", "stop"}
		// 引用字段名按字典序为 attention、work。
		item.ID = Key("attention", item.Kind, q.ID, w.ID)
		return item, true
	case "delivery":
		item.Detail = clip(w.Delivery.Error)
		item.At = cmp.Or(w.Delivery.At, w.UpdatedAt)
		item.Actions = []string{"retry", "mark-delivered", "stop"}
	case "review":
		item.Actions = []string{"stop"}
	case "blocked", "failed":
		item.Actions = []string{"retry", "stop"}
	}
	item.ID = Key("attention", item.Kind, w.ID)
	return item, true
}

const attentionChars = 600

func clip(s string) string { return clipChars(s, attentionChars) }

func clipChars(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i]
		}
		n++
	}
	return s
}
