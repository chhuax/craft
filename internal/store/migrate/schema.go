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
	ordered   bool
	columns   string
	Generated []GeneratedColumn
	Indexes   []IndexSpec
}

var collections = []CollectionSpec{
	{Name: "watch_trigger", Table: "watch_trigger", columns: `, primary_thread TEXT AS (
      json_extract(value,'$.threads[0].source') || ':' || json_extract(value,'$.threads[0].kind') || ':' || json_extract(value,'$.threads[0].id'))`},
	{Name: "watch_disposition", Table: "watch_disposition"},
	{Name: "watch_thread", Table: "watch_thread", ordered: true, columns: `,
      ready INTEGER AS (json_type(value,'$.evaluating') IS NULL AND json_array_length(value,'$.pending') > 0),
      eval_id TEXT AS (json_extract(value,'$.evaluating.id'))`,
		Generated: []GeneratedColumn{{Field: "closed"}},
		Indexes:   []IndexSpec{{Name: "Active", Missing: "closed"}}},
	{Name: "watch_work", Table: "watch_work", ordered: true, columns: `,
      created TEXT AS (json_extract(value,'$.createdAt')),
      origin_id TEXT AS (nullif(json_extract(value,'$.origin.triggerId'),'')), origin_thread TEXT`,
		Generated: []GeneratedColumn{{Field: "ended"}},
		Indexes:   []IndexSpec{{Name: "Active", Missing: "ended"}}},
	{Name: "watch_decision", Table: "watch_decision", columns: `, thread_key TEXT AS (json_extract(value,'$.threadKey'))`},
	{Name: "watch_trigger_thread", Table: "watch_trigger_thread", columns: `,
      trigger_id TEXT AS (json_extract(value,'$[0]')), thread_key TEXT AS (json_extract(value,'$[1]'))`},
	{Name: "watch_event", Table: "watch_event"},
	{Name: "watch_reply_status", Table: "watch_reply_status"},
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
		// 显式命名 INTEGER PRIMARY KEY 使首次插入位置在更新和 VACUUM 后都保持稳定。
		if c.ordered {
			columns = "ordinal INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE, value TEXT NOT NULL CHECK(json_valid(value))"
		}
		columns += c.columns
		for _, col := range c.Generated {
			columns += fmt.Sprintf(", %s TEXT GENERATED ALWAYS AS (json_type(value, '$.%s')) VIRTUAL", col.Field, col.Field)
		}
		if _, err := tx.Exec("CREATE TABLE " + c.Table + " (" + columns + ")"); err != nil {
			return fmt.Errorf("创建集合表 %s: %w", c.Table, err)
		}
		for _, index := range c.Indexes {
			name := c.Table + "_" + strings.ToLower(index.Name)
			query := fmt.Sprintf("CREATE INDEX %s ON %s (%s, ordinal) WHERE %s IS NULL", name, c.Table, index.Missing, index.Missing)
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
			error TEXT, due TEXT NOT NULL, since TEXT NOT NULL, failure_order INTEGER, PRIMARY KEY (kind, key)
		)`,
		"CREATE INDEX effect_order ON watch_effect(kind,seq)",
		"CREATE INDEX effect_due ON watch_effect(kind,due,seq)",
		"CREATE INDEX effect_since ON watch_effect(kind,since)",
		"CREATE INDEX effect_attempts ON watch_effect(kind,attempts) WHERE attempts>0",
		"CREATE INDEX effect_failure ON watch_effect(kind,failure_order) WHERE failure_order IS NOT NULL",
		// SQLite 的按类计数需要遍历该类所有索引项。把行数与队列一起提交，使调度成本与积压深度无关。
		"CREATE TABLE effect_count (kind TEXT PRIMARY KEY, waiting INTEGER NOT NULL) WITHOUT ROWID",
		`CREATE TRIGGER effect_insert AFTER INSERT ON watch_effect BEGIN
			INSERT INTO effect_count VALUES(NEW.kind,1)
			ON CONFLICT(kind) DO UPDATE SET waiting=waiting+1;
		END`,
		`CREATE TRIGGER effect_delete AFTER DELETE ON watch_effect BEGIN
			UPDATE effect_count SET waiting=waiting-1 WHERE kind=OLD.kind;
			DELETE FROM effect_count WHERE kind=OLD.kind AND waiting=0;
		END`,
		"CREATE TABLE alarm_state (" + documentColumns + ")",
		"CREATE TABLE ext_state (" + documentColumns + ")",
	} {
		if _, err := tx.Exec(ddl); err != nil {
			return fmt.Errorf("创建专用表: %w", err)
		}
	}
	return createIndexes(tx)
}
