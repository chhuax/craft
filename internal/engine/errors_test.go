package engine

import (
	"errors"
	"fmt"
	"testing"
)

func TestUserErrors(t *testing.T) {
	for _, tc := range []struct {
		code, want string
		missing    bool
	}{
		{CodeWorkEnded, "workEnded", false},
		{CodeWorkStopped, "workStopped", false},
		{CodeUnknownTrigger, "unknownTrigger", true},
		{CodeUnknownWork, "unknownWork", true},
		{CodeUnknownAssignment, "unknownAssignment", true},
		{CodeAssignmentEnded, "assignmentEnded", false},
		{CodeEvalFinished, "evalFinished", false},
		{CodeWorkerDecides, "workerDecides", false},
		{CodeNotAdmissible, "notAdmissible", false},
		{CodeNotExplicit, "notExplicit", false},
		{CodeNotReached, "notReached", false},
		{CodeHandedBack, "handedBack", false},
		{CodeHandedBackHere, "handedBackHere", false},
		{CodeMandateRevoked, "mandateRevoked", false},
		{CodeNotRetryable, "notRetryable", false},
		{CodeUnknownProblem, "unknownProblem", true},
		{CodeNoAttention, "noAttention", false},
		{CodeThreadAlreadyOwned, "threadAlreadyOwned", false},
		{CodeNoWorkForSession, "noWorkForSession", false},
		{CodeSourceThread, "sourceThread", false},
		{CodeFollowedThread, "followedThread", false},
		{CodeNotFollowing, "notFollowing", false},
		{CodeQuestionWithResult, "questionWithResult", false},
		{CodeAskerWithoutQuestion, "askerWithoutQuestion", false},
		{CodeNotAsker, "notAsker", false},
		{CodeNotAnswerable, "notAnswerable", false},
		{CodeAckEnded, "ackEnded", false},
		{CodeAckNoChannel, "ackNoChannel", false},
		{CodeAckNotPerson, "ackNotPerson", false},
		{CodeNotReady, "notReady", false},
		{CodeNotAttention, "notAttention", false},
		{CodeNotQuestion, "notQuestion", false},
		{CodeNoAnswer, "noAnswer", false},
		{CodeNoChannel, "noChannel", false},
	} {
		t.Run(tc.code, func(t *testing.T) {
			original := &UserError{Code: tc.code, Values: map[string]string{"eval": "e"}}
			err := fmt.Errorf("执行命令: %w", original)
			var got *UserError
			if tc.code != tc.want || !errors.As(err, &got) || got.Values["eval"] != "e" || IsMissing(err) != tc.missing || IsRefused(err) == tc.missing {
				t.Errorf("错误分类或编码错误 %+v", got)
			}
		})
	}
	for _, err := range []error{nil, errors.New("编码失败"), &UserError{Code: "invalid"}} {
		if IsMissing(err) || IsRefused(err) {
			t.Errorf("内部错误被归入业务错误 %v", err)
		}
	}
}
