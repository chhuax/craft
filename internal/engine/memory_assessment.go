package engine

func (r *memoryView) RecentOpenWorks(limit int, exclude ...string) ([]Work, error) {
	if limit <= 0 {
		return nil, nil
	}
	skip := make(map[string]bool, len(exclude))
	for _, id := range exclude {
		skip[id] = true
	}
	var works []Work
	for row := range r.recentOpen.reverseRows() {
		id := string(row.data)
		if skip[id] {
			continue
		}
		w, err := r.GetWork(id)
		if err != nil {
			return nil, err
		}
		works = append(works, *w)
		if len(works) == limit {
			break
		}
	}
	return works, nil
}

func (r *memoryView) ThreadByEval(id string) (*Thread, error) {
	row := r.byEval.get(id)
	if row == nil {
		return nil, nil
	}
	var th Thread
	if err := row.decode(&th); err != nil {
		return nil, err
	}
	return &th, nil
}

func (r *memoryView) AcceptedByTrigger(ids ...string) (map[string]Acceptance, error) {
	found := make(map[string]Acceptance)
	for _, id := range ids {
		for row := range r.accepted.prefixTree(linkPrefix(id)).reverseRows() {
			found[id] = Acceptance{DecisionID: string(row.data), WorkID: row.lookup}
			break
		}
	}
	return found, nil
}

func (r *memoryView) DecisionsByThread(key string) ([]Decision, error) {
	var decisions []Decision
	err := r.decisionsByThread.prefixTree(linkPrefix(key)).walk(func(row *memoryRow) error {
		var d Decision
		if err := row.decode(&d); err != nil {
			return err
		}
		decisions = append(decisions, d)
		return nil
	})
	return decisions, err
}
