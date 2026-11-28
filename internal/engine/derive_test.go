package engine

import "testing"

func reported(state ReportState) Work {
	return Work{ID: "w", Assignments: []Assignment{{State: "running", Report: &Report{State: state, At: at1, Summary: "报告"}}}}
}

func TestWorkStage(t *testing.T) {
	question := &Question{ID: "q", Question: "?"}
	answered := &Question{ID: "q", AnsweredAt: at1}
	ready := reported(Ready)
	blocked := reported(Blocked)
	failed := reported(Failed)
	for _, tc := range []struct {
		name          string
		w             Work
		stage, action string
	}{
		{"结束优先", Work{Ended: &WorkEnd{}, Delivery: &DeliveryFailure{}, Attention: question}, "ended", ""},
		{"交付先于问题和报告", Work{Delivery: &DeliveryFailure{}, Attention: question, Assignments: ready.Assignments}, "person", "delivery"},
		{"问题先于报告", Work{Attention: question, Assignments: failed.Assignments}, "person", "answer"},
		{"已答后检查报告", Work{Attention: answered, Assignments: blocked.Assignments}, "person", "blocked"},
		{"已答后工作中", Work{Attention: answered, Assignments: []Assignment{{State: "running"}}}, "working", ""},
		{"待检查", ready, "person", "review"}, {"阻塞", blocked, "person", "blocked"}, {"失败", failed, "person", "failed"},
		{"就绪后新投递", Work{Assignments: []Assignment{{State: "running", DeliveredAt: at2, Report: ready.Assignments[0].Report}}}, "working", ""},
		{"结束轮次仍待检查", Work{Assignments: []Assignment{{State: "ended", Report: ready.Assignments[0].Report}}}, "person", "review"},
		{"新的人消息解除阻塞", Work{Assignments: blocked.Assignments, Inputs: []WorkInput{{Kind: "trigger", At: at2, Human: true}}}, "working", ""},
		{"只读消息不解除阻塞", Work{Assignments: blocked.Assignments, Inputs: []WorkInput{{Kind: "trigger", At: at2, Human: true, Context: true}}}, "person", "blocked"},
		{"机器消息不解除阻塞", Work{Assignments: blocked.Assignments, Inputs: []WorkInput{{Kind: "trigger", At: at2}}}, "person", "blocked"},
		{"同毫秒普通消息", Work{Assignments: blocked.Assignments, Inputs: []WorkInput{{Kind: "trigger", At: at1, Human: true}}}, "person", "blocked"},
		{"同毫秒答复", Work{Attention: answered, Assignments: blocked.Assignments, Inputs: []WorkInput{{Kind: "trigger", At: at1, Human: true}}}, "working", ""},
		{"尚未派工", Work{}, "working", "queued"},
		{"新轮次待启动", Work{Assignments: []Assignment{failed.Assignments[0], {State: "pending"}}}, "working", "queued"},
		{"等待", reported(Waiting), "idle", ""}, {"轮次结束", Work{Assignments: []Assignment{{State: "ended"}}}, "idle", ""},
		{"进行中", reported(Progress), "working", ""}, {"执行中", Work{Assignments: []Assignment{{State: "running"}}}, "working", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := WorkStage(tc.w); got != (Stage{Stage: tc.stage, Action: tc.action}) {
				t.Fatal(got)
			}
		})
	}
}
