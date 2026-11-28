package app

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/store"
)

const passPeriod = 2 * time.Second
const batchLimit = 100

func (r *Runtime) loop(ctx context.Context, version int64) {
	defer close(r.done)
	timer := r.clock.NewTimer(passPeriod)
	defer func() { timer.Stop() }()
	// 取消只打断等待，已开始的一轮必须在停止 effect 和关库之前完整结束。
	round := context.WithoutCancel(ctx)
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-timer.C:
			// 唤醒流量不能重置轮询期限，否则持续的本地命令会遮住其他进程的提交。
			timer = r.clock.NewTimer(passPeriod)
			current, err := r.repo.db.DataVersion(round)
			if err != nil {
				slog.Error("检查外部写入失败", "error", err)
			} else if current != version {
				version = current
				r.Request()
			}
		}
		if ctx.Err() != nil {
			return
		}
		r.runPass(round)
	}
}

func (r *Runtime) runPass(ctx context.Context) {
	more, err := r.drain(ctx)
	if err != nil {
		slog.Error("处理 inbox 失败", "error", err)
	}
	if _, err := r.pass.EvaluateDue(ctx); err != nil {
		slog.Error("研判待处理对象失败", "error", err)
	}
	r.effects.Kick()
	if more {
		r.Request()
	}
}

func (r *Runtime) drain(ctx context.Context) (bool, error) {
	var pending bool
	err := r.repo.db.View(ctx, func(read store.SQLiteReader) error {
		var err error
		pending, err = read.HasInbox()
		return err
	})
	if err != nil || !pending {
		return false, err
	}
	var more bool
	err = r.repo.db.Write(ctx, func(tx *store.SQLiteTx) error {
		rows, err := tx.InboxBatch(batchLimit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			err := tx.Savepoint(ctx, func(child *store.SQLiteTx) error { return r.consume(ctx, child, row) })
			if err == nil {
				continue
			}
			if !errors.Is(err, errPoison) {
				return fmt.Errorf("消费 inbox[%q]: %w", row.ID, err)
			}
			// 保存点已撤销该行的全部引擎写入和广播。删除坏行以免每拍都阻塞后续事件。
			if delErr := tx.DeleteInbox(row.ID); delErr != nil {
				return errors.Join(err, delErr)
			}
			tx.AfterCommit(func() { slog.Error("已移除损坏的 inbox 行", "id", row.ID, "seq", row.Seq, "error", err) })
		}
		// 不足一批已读到队尾；后来入队的行有自己的提交唤醒或外部版本通知。
		if len(rows) == batchLimit {
			more, err = tx.HasInbox()
		}
		return err
	})
	return err == nil && more, err
}

var errPoison = errors.New("事件数据损坏")

func (r *Runtime) consume(ctx context.Context, tx *store.SQLiteTx, row store.InboxRow) error {
	var tr engine.Trigger
	if err := json.Unmarshal(row.Value, &tr); err != nil {
		return fmt.Errorf("%w: %w", errPoison, err)
	}
	if tr.ID == "" || tr.ID != row.ID || tr.At != row.At || len(tr.Threads) == 0 || (tr.Source == "craft" && tr.Event == "person.answer") {
		return fmt.Errorf("%w: 事件标识、时间、类型或主对象无效", errPoison)
	}
	if _, err := engine.IsoTime(tr.At); err != nil {
		return fmt.Errorf("%w: %w", errPoison, err)
	}
	result, err := r.pass.ReceiveIn(r.repo.in(ctx, tx), tr)
	if err != nil {
		// 关联文档也可能损坏。只跳过可定位到该行的数据错误；计数器、磁盘和事务故障必须整批重试。
		_, semantic := errors.AsType[*json.SemanticError](err)
		_, syntax := errors.AsType[*jsontext.SyntacticError](err)
		_, refused := errors.AsType[*engine.UserError](err)
		if semantic || syntax || refused {
			return fmt.Errorf("%w: %w", errPoison, err)
		}
		return err
	}
	if result == engine.Filtered || result == engine.Duplicate {
		tx.AfterCommit(func() { slog.Debug("inbox 事件处置", "id", row.ID, "result", result) })
	}
	return tx.DeleteInbox(row.ID)
}
