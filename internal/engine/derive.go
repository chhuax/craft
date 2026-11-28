package engine

// Stage 区分工作的总体阶段和当前需要人的操作。
type Stage struct {
	Stage  string `json:"stage"`
	Action string `json:"action,omitempty"`
}

// WorkStage 根据当前事实取第一个命中的阶段；重试派工后只看最新轮次的报告。
func WorkStage(w Work) Stage {
	switch {
	case w.Ended != nil:
		return Stage{Stage: "ended"}
	case w.Delivery != nil:
		return Stage{"person", "delivery"}
	case w.Attention != nil && w.Attention.AnsweredAt == "":
		return Stage{"person", "answer"}
	case len(w.Assignments) == 0:
		return Stage{"working", "queued"}
	}
	a := &w.Assignments[len(w.Assignments)-1]
	if r := a.Report; r != nil {
		switch r.State {
		case Ready:
			if a.DeliveredAt > r.At {
				return Stage{Stage: "working"}
			}
			return Stage{"person", "review"}
		case Blocked, Failed:
			if !answeredSince(&w, r.At) {
				return Stage{"person", string(r.State)}
			}
		}
	}
	if a.State == AssignmentPending {
		return Stage{"working", "queued"}
	}
	if a.State == AssignmentEnded || (a.Report != nil && a.Report.State == Waiting) {
		return Stage{Stage: "idle"}
	}
	return Stage{Stage: "working"}
}

func answeredSince(w *Work, at string) bool {
	for i := range w.Inputs {
		input := &w.Inputs[i]
		if input.Kind != "trigger" || !input.Human || input.Context {
			continue
		}
		// 普通消息与报告同毫秒时无法确定先后，明确回答当前问题才可解除等待。
		if input.At > at || (input.At == at && w.Attention != nil && w.Attention.AnsweredAt != "" && w.Attention.AnsweredAt == at) {
			return true
		}
	}
	return false
}
