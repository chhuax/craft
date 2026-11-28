package engine

// PolicyEffects 比较在办工作的原始授权，只返回需要收到变更输入的工作。
func PolicyEffects(openWorks []Work, old, next Policy) map[string]PolicyEffect {
	effects := make(map[string]PolicyEffect)
	for i := range openWorks {
		w := &openWorks[i]
		m := &w.Mandate
		rules := changedRules(m.Basis.Rules, old, next)
		effect := PolicyEffect{Rules: rules}
		if old.Revalidate(*m) && !next.Revalidate(*m) {
			mode := next.ModeOf(m.Basis.Source, m.Basis.Event)
			// 旧策略已通过校验，把模式和规则恢复后，只有目录缺项或发送人失权仍能导致失败。
			no := false
			switch {
			case mode == "" || !next.senderValid(m):
				actor := m.Actor
				effect.RequesterAllowed = &no
				effect.Requester = &actor
			case m.Strength == Inferred && mode != "auto":
				effect.Proactive = &no
			}
		}
		if len(rules) > 0 || effect.Proactive != nil || effect.RequesterAllowed != nil {
			effects[w.ID] = effect
		}
	}
	return effects
}

func changedRules(ids []string, old, next Policy) []RuleChange {
	var changes []RuleChange
	for _, id := range ids {
		before, after := old.rulesByID[id], next.rulesByID[id]
		switch {
		case before.Enabled && !after.Enabled:
			changes = append(changes, RuleChange{ID: id, Name: before.Name, Change: "disabled"})
		case after.Enabled && before != after:
			changes = append(changes, RuleChange{ID: id, Name: after.Name, Change: "changed", Instruction: &after.Instruction})
		}
	}
	return changes
}
