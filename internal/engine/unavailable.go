package engine

import (
	"cmp"
	"strings"
)

// 同一毫秒没有更细的时序，按决策标识打破平局，避免结果随查询顺序变化。
func newer(d, other *Decision) bool {
	return other == nil || d.At > other.At || (d.At == other.At && d.ID > other.ID)
}

func threadWarning(th *Thread, decisions []*Decision) (AttentionItem, bool) {
	if len(decisions) == 0 {
		return AttentionItem{}, false
	}
	latest := make(map[string]*Decision, len(decisions))
	for _, d := range decisions {
		for _, id := range d.TriggerIDs {
			if newer(d, latest[id]) {
				latest[id] = d
			}
		}
	}
	// pending 和 evaluating 已经回到处理流程，不应再催人重试。
	for _, id := range th.Pending {
		delete(latest, id)
	}
	if th.Evaluating != nil {
		for _, id := range th.Evaluating.TriggerIDs {
			delete(latest, id)
		}
	}
	var best *Decision
	var trigger string
	for _, d := range decisions {
		if d.Kind != Unavailable || d.Dismissed || !newer(d, best) || internalRequeue(d.Reason) {
			continue
		}
		for _, r := range d.Reason {
			if r.Code == ReasonRouted && latest[r.TriggerID] == d {
				delete(latest, r.TriggerID)
			}
		}
		// 同批有的事件已处理，只把第一个仍未处理的交给操作方。
		for _, id := range d.TriggerIDs {
			if latest[id] == d {
				best = d
				trigger = id
				break
			}
		}
	}
	if best == nil {
		return AttentionItem{}, false
	}
	ref := AttentionRef{Decision: best.ID, Thread: th.Key, Trigger: trigger}
	return AttentionItem{
		ID:   Key("attention", "unavailable", ref.Decision, ref.Thread, ref.Trigger),
		Kind: "unavailable", Severity: "warning", Ref: ref, Title: clip(cmp.Or(th.Meta.Label, th.Key)),
		Detail: reasonDetail(best.Reason), Actions: []string{"reevaluate", "dismiss"}, At: best.At,
	}, true
}

func internalRequeue(reasons []Reason) bool {
	for _, r := range reasons {
		switch r.Code {
		case ReasonPolicyChanged, ReasonAcceptedBefore, "mainRebuilt", "watchPaused":
		default:
			return false
		}
	}
	return len(reasons) > 0
}

func reasonDetail(reasons []Reason) string {
	// 只取可显示的前缀，避免把超长的原文或整个原因列表拼好后才截断。
	var b strings.Builder
	left := attentionChars
	for i, r := range reasons {
		value := r.Code
		if r.Code == ReasonSaid {
			value = r.Values["text"]
		}
		prefix := ""
		if i > 0 {
			prefix = "；"
		}
		separator := ""
		if r.TriggerID != "" {
			separator = ": "
		}
		for _, part := range [...]string{prefix, r.TriggerID, separator, value} {
			for _, ch := range part {
				if left == 0 {
					return b.String()
				}
				b.WriteRune(ch)
				left--
			}
		}
	}
	return b.String()
}
