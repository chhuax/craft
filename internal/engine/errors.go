package engine

import (
	"errors"
	"regexp"
)

// CLI 以这些稳定的码区分引擎拒绝原因；显示文案可以独立演进。
const (
	CodeWorkEnded            = "workEnded"
	CodeWorkStopped          = "workStopped"
	CodeUnknownTrigger       = "unknownTrigger"
	CodeUnknownWork          = "unknownWork"
	CodeUnknownAssignment    = "unknownAssignment"
	CodeAssignmentEnded      = "assignmentEnded"
	CodeEvalFinished         = "evalFinished"
	CodeWorkerDecides        = "workerDecides"
	CodeNotAdmissible        = "notAdmissible"
	CodeNotExplicit          = "notExplicit"
	CodeNotReached           = "notReached"
	CodeHandedBack           = "handedBack"
	CodeHandedBackHere       = "handedBackHere"
	CodeMandateRevoked       = "mandateRevoked"
	CodeNotRetryable         = "notRetryable"
	CodeUnknownProblem       = "unknownProblem"
	CodeNoAttention          = "noAttention"
	CodeThreadAlreadyOwned   = "threadAlreadyOwned"
	CodeNoWorkForSession     = "noWorkForSession"
	CodeSourceThread         = "sourceThread"
	CodeFollowedThread       = "followedThread"
	CodeNotFollowing         = "notFollowing"
	CodeQuestionWithResult   = "questionWithResult"
	CodeAskerWithoutQuestion = "askerWithoutQuestion"
	CodeNotAsker             = "notAsker"
	CodeNotAnswerable        = "notAnswerable"
	CodeAckEnded             = "ackEnded"
	CodeAckNoChannel         = "ackNoChannel"
	CodeAckNotPerson         = "ackNotPerson"
	CodeNotReady             = "notReady"
	CodeNotAttention         = "notAttention"
	CodeNotQuestion          = "notQuestion"
	CodeNoAnswer             = "noAnswer"
	CodeNoChannel            = "noChannel"
)

var userErrorMessages = map[string]string{
	CodeWorkEnded:            "工作已结束",
	CodeWorkStopped:          "工作已被人停止，需要重新接受",
	CodeUnknownTrigger:       "找不到这条事件",
	CodeUnknownWork:          "找不到这件工作",
	CodeUnknownAssignment:    "找不到这次派工",
	CodeAssignmentEnded:      "这次派工已结束",
	CodeEvalFinished:         "研判 {eval} 已结束或被取代",
	CodeWorkerDecides:        "消息已到达工作 {works}，由执行方判断归属",
	CodeNotAdmissible:        "这条事件不能发起工作：{reason}",
	CodeNotExplicit:          "推断候选须按启用的自动规则接受",
	CodeNotReached:           "事件没有到达这件工作",
	CodeHandedBack:           "这件工作已退回过该事件",
	CodeHandedBackHere:       "请将已退回的事件作为新工作接受、另行挂接或忽略",
	CodeMandateRevoked:       "当前策略已撤销授权",
	CodeNotRetryable:         "当前工作状态不允许重试",
	CodeUnknownProblem:       "找不到这条不可用的研判记录",
	CodeNoAttention:          "工作没有待回答的问题",
	CodeThreadAlreadyOwned:   "对象 {threads} 已由其他在办工作持有",
	CodeNoWorkForSession:     "当前会话没有对应的在办工作",
	CodeSourceThread:         "起源对象须保持订阅",
	CodeFollowedThread:       "自有对象须保持订阅",
	CodeNotFollowing:         "工作没有订阅该对象",
	CodeQuestionWithResult:   "问题与就绪结果或回答须分别报告",
	CodeAskerWithoutQuestion: "提供提问来源时须同时给出问题",
	CodeNotAsker:             "问题须指向已到达的人的消息",
	CodeNotAnswerable:        "回答须指向已到达的人的问题",
	CodeAckEnded:             "工作结束后不能再确认历史请求",
	CodeAckNoChannel:         "确认需要有回复渠道的人的消息",
	CodeAckNotPerson:         "确认需要原始的人的消息",
	CodeNotReady:             "工作尚未就绪",
	CodeNotAttention:         "只能发送当前待人回答的问题",
	CodeNotQuestion:          "回答须指向原始的人的问题",
	CodeNoAnswer:             "执行方尚未回答这条消息",
	CodeNoChannel:            "工作或事件缺少回复渠道",
}

var errorParam = regexp.MustCompile(`\{\w+\}`)

// UserError 仅表示规格列出的业务错误，请以指针返回，供 errors.As 获取码与参数。
// 参数类型错误、事件缺少对象等编程错误直接返回内部错误，不包装为 UserError。
type UserError struct {
	Code   string
	Values map[string]string
}

// Error 给出人可读原因。保留未提供的占位符，便于定位缺失的显示参数。
func (e *UserError) Error() string {
	message, ok := userErrorMessages[e.Code]
	if !ok {
		return "未知引擎错误码 " + e.Code
	}
	return errorParam.ReplaceAllStringFunc(message, func(key string) string {
		if v, ok := e.Values[key[1:len(key)-1]]; ok {
			return v
		}
		return key
	})
}

// IsMissing 识别经包装的对象缺失错误，对应 CLI 退出码 5。
func IsMissing(err error) bool {
	var e *UserError
	if !errors.As(err, &e) || e == nil {
		return false
	}
	return e.missing()
}

func (e *UserError) missing() bool {
	switch e.Code {
	case CodeUnknownWork, CodeUnknownTrigger, CodeUnknownAssignment, CodeUnknownProblem:
		return true
	}
	return false
}

// IsRefused 识别经包装的业务拒绝，也对应退出码 5。
func IsRefused(err error) bool {
	var e *UserError
	return errors.As(err, &e) && e != nil && userErrorMessages[e.Code] != "" && !e.missing()
}
