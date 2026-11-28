package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"unicode/utf8"

	"github.com/chhuax/craft/internal/store/migrate"
)

const (
	// All 适用于所有文档集合。
	All = ""
	// Active 仅适用于声明了该部分索引的集合。
	Active = "Active"
)

// Store 提供独立的事务。回调内需要嵌套写入时，使用传入的 Tx.Atomic。
type Store interface {
	// Atomic 以写事务执行 fn，只有 fn 成功且提交成功时才运行提交回调。
	Atomic(ctx context.Context, fn func(Tx) error) error
	// Read 提供一致的已提交数据快照，可在另一个写事务进行期间使用。
	Read(ctx context.Context, fn func(ReadTx) error) error
}

// ReadTx 仅在当前回调内使用；返回的 JSON 字节归调用方所有。
type ReadTx interface {
	// Get 用 found 区分缺失的键与值为 JSON null 的文档。
	Get(collection, key string) (value json.RawMessage, found bool, err error)
	// List 按键返回符合命名谓词的文档，使用 All 可读取整个集合。
	List(collection, filter string) (map[string]json.RawMessage, error)
}

// Doc 是 PutMany 的一项输入，Value 必须是有效的 UTF-8 JSON。
type Doc struct {
	Key   string
	Value json.RawMessage
}

// Tx 供当前回调在同一 goroutine 使用；嵌套回调只使用自己的 Tx。
type Tx interface {
	ReadTx
	// Put 保留原始 JSON，包括未知字段和数值精度；同键写入替换整份文档。
	Put(collection, key string, value json.RawMessage) error
	// PutMany 先校验整批文档，再按切片顺序写入，同键以后者为准。
	// 空切片直接成功；集合或文档非法时，本次调用不留下写入。
	PutMany(collection string, docs []Doc) error
	// Delete 允许键缺失，重复删除仍可提交。
	Delete(collection, key string) error
	// Atomic 将内层修改和回调一同并入父事务，失败时仅撤销内层。
	Atomic(ctx context.Context, fn func(Tx) error) error
	// AfterCommit 的回调按注册顺序在最外层成功提交并释放写锁后运行。
	// fn 内的普通错误由回调记录；回调 panic 时仓储记日志并继续执行后面的回调。
	AfterCommit(fn func())
}

type callbacks []func()

func (c *callbacks) AfterCommit(fn func()) {
	*c = append(*c, fn)
}

func (c callbacks) run(logger *slog.Logger) {
	for _, fn := range c {
		// 每个回调单独恢复，前一个广播失败时仍需唤醒后面的使用者。
		func() {
			defer func() {
				if failure := recover(); failure != nil {
					logger.Error("提交回调失败", "error", failure)
				}
			}()
			fn()
		}()
	}
}

func validateJSON(collection, key string, value json.RawMessage) error {
	if !utf8.Valid(value) || !json.Valid(value) {
		return fmt.Errorf("集合 %q 键 %q 的值不是有效 JSON", collection, key)
	}
	return nil
}

func validateDocs(collection string, docs []Doc) (string, error) {
	table, _, err := migrate.Resolve(collection, All)
	if err != nil {
		return "", err
	}
	for _, doc := range docs {
		if err := validateJSON(collection, doc.Key, doc.Value); err != nil {
			return "", err
		}
	}
	return table, nil
}
