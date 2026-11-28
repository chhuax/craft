package app

import (
	"context"

	jsonv2 "encoding/json/v2"
	"fmt"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

// SourceContext 将一个扩展的接收入口限定在它声明的事件及对象目录内，可并发使用。
type SourceContext struct {
	id    string
	scope exthost.SourceScope
	db    *store.SQLite
	wake  func()
}

// NewSourceContext 从已装载的 manifest 复制声明。db 由调用方管理，wake 须为非阻塞的置位操作。
func NewSourceContext(m exthost.Manifest, db *store.SQLite, wake func()) (*SourceContext, error) {
	if db == nil || wake == nil {
		return nil, fmt.Errorf("接收入口需要数据库及提交唤醒函数")
	}
	return &SourceContext{id: m.ID, scope: exthost.NewSourceScope(m), db: db, wake: wake}, nil
}

// Receive 处理 watch/receive。只有文件事务提交后，扩展才可根据结果推进游标。
func (s *SourceContext) Receive(ctx context.Context, p protocol.Trigger) (string, error) {
	p, err := s.scope.Normalize(p)
	if err != nil {
		return "", s.invalid(err)
	}
	return s.receive(ctx, p)
}

func (s *SourceContext) receive(ctx context.Context, p protocol.Trigger) (string, error) {
	tr := engineTrigger(p)
	value, err := jsonv2.Marshal(&tr)
	if err != nil {
		return "", s.invalid(err)
	}
	result := "duplicate"
	err = s.db.Write(ctx, func(tx *store.SQLiteTx) error {
		// 两次查重之间不能释放写锁，否则 drain 可能把同一 ID 从 inbox 移入 Trigger 表。
		_, found, err := tx.Get("watch_trigger", tr.ID)
		if err != nil {
			return err
		}
		if !found {
			added, err := tx.InsertInbox(tr.ID, value, tr.At)
			if err != nil {
				return err
			}
			if added {
				result = "queued"
			}
		}
		tx.AfterCommit(s.wake)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("扩展 %q 接收事件 %q: %w", s.id, tr.ID, err)
	}
	return result, nil
}

func (s *SourceContext) invalid(err error) error {
	return &protocol.Error{Code: -32602, Message: fmt.Sprintf("扩展 %q 的 watch/receive 参数无效: %v", s.id, err)}
}

func engineRef(ref protocol.ThreadRef) engine.ThreadRef {
	return engine.NewThreadRef(ref.Source, ref.Kind, ref.ID)
}

func engineTrigger(p protocol.Trigger) engine.Trigger {
	tr := engine.Trigger{ID: p.ID, Source: p.Source, Event: p.Event, At: p.At, Human: p.Human, Text: p.Text, Meta: p.Meta,
		Actor: engine.ActorRef{Source: p.Actor.Source, ID: p.Actor.ID}}
	for _, ref := range p.Threads {
		tr.Threads = append(tr.Threads, engineRef(ref))
	}
	if p.Addressed != nil {
		tr.Addressed = *p.Addressed
	}
	if p.Actor.Name != nil {
		tr.Actor.Name = *p.Actor.Name
	}
	if p.URL != nil {
		tr.URL = *p.URL
	}
	if p.Channel != nil {
		tr.Channel = &engine.ChannelRef{Source: p.Channel.Source, Thread: p.Channel.Thread, Extra: p.Channel.Extra}
	}
	return tr
}
