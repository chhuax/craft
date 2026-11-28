package engine

// Reason 保存可本地化的原因码和替换值，可定位到批内的单个事件。
type Reason struct {
	Code      string            `json:"code"`
	Values    map[string]string `json:"values,omitempty"`
	TriggerID string            `json:"triggerId,omitempty"`
}

// 原因码是持久化契约；展示层按码和参数选择文案。
const (
	ReasonSaid            = "said"
	ReasonTimeout         = "timeout"
	ReasonPolicyChanged   = "policyChanged"
	ReasonNoAdmissible    = "noAdmissible"
	ReasonStartedWork     = "startedWork"
	ReasonAcceptedBefore  = "acceptedBefore"
	ReasonRouted          = "routed"
	ReasonHumanStop       = "humanStop"
	ReasonStopUnknown     = "stopUnknown"
	ReasonStopNotExplicit = "stopNotExplicit"
	ReasonIgnoredFromCard = "ignoredFromCard"
	ReasonObserved        = "observed"
	ReasonRetried         = "retried"
	ReasonHostRestarted   = "hostRestarted"
	ReasonUnknownEvent    = "unknownEvent"
	ReasonFollowupOnly    = "followupOnly"
	ReasonModeOff         = "modeOff"
	ReasonSourceOff       = "sourceOff"
	ReasonUnknownSource   = "unknownSource"
)
