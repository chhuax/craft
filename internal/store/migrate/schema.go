package migrate

import (
	"database/sql"
	"fmt"
	"strings"
)

// GeneratedColumn 以顶层 JSON 字段命名，保存其 JSON 类型以区分缺失和显式 null。
type GeneratedColumn struct {
	Field string
}

// IndexSpec 给字段缺失谓词命名，使查询可以复用建索引时的条件。
type IndexSpec struct {
	Name    string
	Missing string
}

// CollectionSpec 声明文档集合的逻辑名、物理表及可供 List 使用的索引。
type CollectionSpec struct {
	Name      string
	Table     string
	Generated []GeneratedColumn
	Indexes   []IndexSpec
}

var collections = []CollectionSpec{
	{Name: "watch_trigger", Table: "watch_trigger"},
	{Name: "watch_disposition", Table: "watch_disposition"},
	{Name: "watch_thread", Table: "watch_thread",
		Generated: []GeneratedColumn{{Field: "closed"}},
		Indexes:   []IndexSpec{{Name: "Active", Missing: "closed"}}},
	{Name: "watch_work", Table: "watch_work",
		Generated: []GeneratedColumn{{Field: "ended"}},
		Indexes:   []IndexSpec{{Name: "Active", Missing: "ended"}}},
	{Name: "watch_decision", Table: "watch_decision"},
	{Name: "watch_trigger_thread", Table: "watch_trigger_thread"},
	{Name: "watch_event", Table: "watch_event"},
	{Name: "watch_kv", Table: "watch_kv"},
}

// Resolve 只返回注册表中的标识符，避免把调用方的集合名或筛选条件直接拼入 SQL。
// 空筛选返回空字段名，命名筛选返回要求缺失的顶层字段。
func Resolve(collection, filter string) (table, missing string, err error) {
	for _, c := range collections {
		if c.Name != collection {
			continue
		}
		if filter == "" {
			return c.Table, "", nil
		}
		for _, index := range c.Indexes {
			if index.Name == filter {
				return c.Table, index.Missing, nil
			}
		}
		return "", "", fmt.Errorf("集合 %q 没有谓词 %q", collection, filter)
	}
	return "", "", fmt.Errorf("未注册的文档集合 %q", collection)
}

const documentColumns = "key TEXT PRIMARY KEY, value TEXT NOT NULL CHECK(json_valid(value))"

func createV1(tx *sql.Tx) error {
	for _, c := range collections {
		columns := documentColumns
		for _, col := range c.Generated {
			columns += fmt.Sprintf(", %s TEXT GENERATED ALWAYS AS (json_type(value, '$.%s')) VIRTUAL", col.Field, col.Field)
		}
		if _, err := tx.Exec("CREATE TABLE " + c.Table + " (" + columns + ")"); err != nil {
			return fmt.Errorf("创建集合表 %s: %w", c.Table, err)
		}
		for _, index := range c.Indexes {
			name := c.Table + "_" + strings.ToLower(index.Name)
			query := fmt.Sprintf("CREATE INDEX %s ON %s (key) WHERE %s IS NULL", name, c.Table, index.Missing)
			if _, err := tx.Exec(query); err != nil {
				return fmt.Errorf("创建索引 %s: %w", name, err)
			}
		}
	}

	// 这四张表各有专属写入方，因此不登记为可由通用接口写入的集合。
	for _, ddl := range []string{
		`CREATE TABLE watch_inbox (
			id TEXT PRIMARY KEY, value TEXT NOT NULL, at TEXT NOT NULL, seq INTEGER NOT NULL
		)`,
		`CREATE TABLE watch_effect (
			kind TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL CHECK(json_valid(value)),
			seq INTEGER NOT NULL, generation INTEGER NOT NULL, attempts INTEGER NOT NULL,
			error TEXT, due TEXT NOT NULL, since TEXT NOT NULL, PRIMARY KEY (kind, key)
		)`,
		"CREATE TABLE alarm_state (" + documentColumns + ")",
		"CREATE TABLE ext_state (" + documentColumns + ")",
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("创建专用表: %w", err)
		}
	}
	return nil
}
