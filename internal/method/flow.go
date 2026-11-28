package method

// Stage 是展示用的进度节点。State 取 pending、done、skipped 或 stale。
type Stage struct {
	Name, State string
}

// Flow 按工作类型、当前产物和确认记录现算文档进度。后面的节点可以独立完成，结果只供展示。
func Flow(kind string, files map[string]Document, c *Confirmation, designSkipped bool) []Stage {
	if !validType(kind) {
		return nil
	}
	req := files[requirementsFile]
	design := files[designFile]
	main, combined := files[combinedFile]
	if combined {
		req, design = main, main
	}
	var stages []Stage
	add := func(name string, done, skipped bool) {
		state := "pending"
		if done {
			state = "done"
		} else if skipped {
			state = "skipped"
		}
		stages = append(stages, Stage{name, state})
	}
	tier, _ := req.value("tier").(string)
	if combined {
		tier = "light"
	}
	required, plan := requiredSections(kind, tier)
	for _, name := range required {
		done := req.hasText(name)
		if name == "验收场景" {
			done = len(req.Scenarios) > 0
		}
		add(name, done, false)
	}
	for _, name := range plan {
		done := design.hasText(name)
		switch name {
		case "结论":
			if kind != "explore" {
				name = "方案"
			}
		case "验证":
			// 验证表只说明如何验证，不能把计划显示成一次已通过的验证。
			name = "验证计划"
			done = len(req.Scenarios) > 0 && len(design.Verification) > 0 && len(checkCoverage(req, design)) == 0
		}
		add(name, done, designSkipped)
	}
	stages = append(stages, Stage{"确认", confirmationStage(files, c, designSkipped)})
	return stages
}

func confirmationStage(files map[string]Document, c *Confirmation, skipped bool) string {
	if skipped {
		return "skipped"
	}
	if c == nil {
		return "pending"
	}
	var docs []Document
	for _, name := range []string{combinedFile, requirementsFile, designFile} {
		if d, ok := files[name]; ok {
			docs = append(docs, d)
		}
	}
	// 已确认的单份草稿之外若新增了方案，它也必须在人看过的集合里。
	_, combined := files[combinedFile]
	want := 2
	if combined {
		want = 1
	}
	if len(docs) == want && len(c.Docs) == want && Verify(c, docs...).State == "confirmed" {
		return "done"
	}
	return "stale"
}
