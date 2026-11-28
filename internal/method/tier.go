package method

import "fmt"

// RiskFacts 是调用方已核实的事实。接口与库表表示契约确有改动，数量为实际条数。
type RiskFacts struct {
	Interface, Schema, Config bool
	CrossRepo, PublicModule   bool
	Behaviors, Rules          int
}

// SuggestTier 返回最高的适用档位和每条命中的依据。规则数区间取较谨慎的六条上限。
func SuggestTier(f RiskFacts) (string, []string) {
	tier := "light"
	var reasons []string
	if f.Config {
		tier = "standard"
		reasons = append(reasons, "配置有改动")
	}
	if f.Behaviors > 3 {
		tier = "standard"
		reasons = append(reasons, fmt.Sprintf("有 %d 条行为，超过轻档的 3 条", f.Behaviors))
	}
	for _, r := range []struct {
		hit    bool
		reason string
	}{
		{f.Interface, "接口契约有改动"}, {f.Schema, "库表有改动"}, {f.CrossRepo, "涉及多个仓库"}, {f.PublicModule, "公共模块有改动"},
	} {
		if r.hit {
			tier = "heavy"
			reasons = append(reasons, r.reason)
		}
	}
	if f.Rules > 6 {
		tier = "heavy"
		reasons = append(reasons, fmt.Sprintf("有 %d 条业务规则，超过 6 条", f.Rules))
	}
	if len(reasons) == 0 {
		reasons = []string{fmt.Sprintf("行为 %d 条、规则 %d 条，无接口、库表、配置、跨仓库或公共模块改动", f.Behaviors, f.Rules)}
	}
	return tier, reasons
}
