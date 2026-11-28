package engine

import (
	"reflect"
	"testing"
)

func TestFollowsDone(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	first := ThreadClose{At: at1, Outcome: "closed"}
	last := ThreadClose{At: at2, Outcome: "done", Detail: "已验收"}
	for _, tc := range []struct {
		name    string
		follows []Follow
		report  ReportState
		want    *ThreadClose
	}{
		{"无跟随无报告", nil, "", nil},
		{"无跟随未就绪", nil, Blocked, nil},
		{"无跟随就绪", nil, Ready, &ThreadClose{At: at2, Outcome: "completed"}},
		{"自有对象尚未关闭", []Follow{{Own: true}}, Ready, nil},
		{"自有对象部分关闭", []Follow{{Own: true, Closed: &last}, {Own: true}}, Ready, nil},
		{"自有对象全部关闭", []Follow{{Own: true, Closed: &last}, {Own: true, Closed: &first}, {Own: false}}, Ready, &last},
		{"关闭由对象确认", []Follow{{Own: true, Closed: &first}}, "", &first},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Work{Follows: tc.follows}
			if tc.report != "" {
				w.Assignments = []Assignment{{Report: &Report{State: tc.report, At: at2}}}
			}
			if got := followsDone(&w); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("结束事实 %+v，期望 %+v", got, tc.want)
			}
		})
	}
}

func TestUnfinishedAfterClose(t *testing.T) {
	t.Setenv("CRAFT_HOME", t.TempDir())
	for _, tc := range []struct {
		name, created string
		report        ReportState
		queued        bool
		input         string
		want          bool
	}{
		{"无派工", "", "", false, "", false}, {"关闭时已有派工", at1, Progress, false, "", false},
		{"后来派工未报告", at2, "", false, "", true}, {"后来派工未完成", at2, Progress, false, "", true},
		{"后来派工就绪", at2, Ready, false, "", false}, {"仍待投递", at2, Ready, true, "", true},
		{"报告后的新输入", at2, Ready, false, "trigger", true}, {"报告后的策略输入", at2, Ready, false, "policy-changed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := Work{}
			if tc.created != "" {
				a := Assignment{CreatedAt: tc.created}
				if tc.report != "" {
					a.Report = &Report{State: tc.report, At: at2}
				}
				w.Assignments = []Assignment{a}
			}
			if tc.queued {
				w.Undelivered = []string{"pending"}
			}
			if tc.input != "" {
				w.Inputs = []WorkInput{{Kind: tc.input, At: "2027-01-01T00:00:00.000Z"}}
			}
			if got := unfinished(&w, at1); got != tc.want {
				t.Errorf("拖住结束 %v，期望 %v", got, tc.want)
			}
		})
	}
}
