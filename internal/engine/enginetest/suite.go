// Package enginetest 固定引擎命令的可观察行为，供各仓储适配器共用。
package enginetest

import (
	"testing"

	"github.com/chhuax/craft/internal/engine"
)

const now = "2026-01-02T00:00:00.000Z"

// Run 为每组命令提供独立空仓储；持久化适配器应在 factory 中使用 t.TempDir 建库。
func Run(t *testing.T, factory func(t *testing.T) engine.Store) {
	t.Helper()
	for _, tc := range []struct {
		name  string
		check func(*testing.T, engine.Store)
	}{
		{"研判攒批与重排", evaluationBatch},
		{"暂停研判", evaluationPaused},
		{"研判预检", evaluationPreflight},
		{"同毫秒的后续批次", nextBatch},
		{"接单核对命中规则", admissionRules},
		{"独立研判事务", independentEvals},
		{"接单与回滚", decideAccept},
		{"研判决策", decisionModes},
		{"跟进归属与停止决策", workerDecides},
		{"自有对象互斥", ownExclusion},
		{"候选卡片", candidateCards},
		{"规则研判", rulesAssessment},
		{"人工重判", reevaluate},
		{"挂接与恢复", attachBatch},
		{"挂接拒绝", attachRefusals},
		{"挂接时的恢复准入", attachRecoveryGate},
		{"批内已接事件", acceptedDuringEval},
		{"恢复时的自有对象互斥", ownershipOnResume},
		{"自有对象能力和关闭事实", ownCapabilities},
		{"对象状态事实", threadStatus},
		{"对象描述与补丁", threadMeta},
		{"接收与未认领路由", receiveUnclaimed},
		{"接收参数错误", receiveInvalid},
		{"订阅到达与恢复", receiveReached},
		{"授权撤销的跟进事件", receiveRevokedFollowup},
		{"多对象分发、回滚与重放", receiveAtomic},
	} {
		t.Run(tc.name, func(t *testing.T) { tc.check(t, factory(t)) })
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newEngine(t *testing.T, cfg engine.Config) *engine.Engine {
	t.Helper()
	e, err := engine.New(cfg)
	must(t, err)
	return e
}

func config(s engine.Store) engine.Config {
	return engine.Config{Store: s, Clock: func() string { return now }, Policy: func() engine.Policy { return engine.Policy{} }}
}
