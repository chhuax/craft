package app

import (
	"context"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"fmt"
	"maps"

	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
	"github.com/chhuax/craft/pkg/protocol"
)

// SourceContext 将一个扩展的接收入口限定在它声明的事件及对象目录内，可并发使用。
type SourceContext struct {
	id            string
	events, kinds map[[2]string]bool
	db            *store.SQLite
	wake          func()
}

// NewSourceContext 从已装载的 manifest 复制声明。db 由调用方管理，wake 须为非阻塞的置位操作。
func NewSourceContext(m exthost.Manifest, db *store.SQLite, wake func()) (*SourceContext, error) {
	if db == nil || wake == nil {
		return nil, fmt.Errorf("接收入口需要数据库及提交唤醒函数")
	}
	s := &SourceContext{id: m.ID, events: make(map[[2]string]bool), kinds: make(map[[2]string]bool), db: db, wake: wake}
	for _, src := range m.WatchSources {
		for _, ev := range src.Events {
			s.events[[2]string{src.ID, ev.Event}] = true
		}
		for _, k := range src.ThreadKinds {
			s.kinds[[2]string{src.ID, k.ID}] = true
		}
	}
	return s, nil
}

// Receive 处理 watch/receive。只有文件事务提交后，扩展才可根据结果推进游标。
func (s *SourceContext) Receive(ctx context.Context, p protocol.Trigger) (string, error) {
	tr, err := s.normalize(p)
	if err != nil {
		return "", s.invalid(err)
	}
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

func (s *SourceContext) normalize(p protocol.Trigger) (engine.Trigger, error) {
	var tr engine.Trigger
	switch {
	case p.ID == "":
		return tr, fmt.Errorf("缺少事件 ID")
	case p.Source == exthost.ReservedSource:
		return tr, fmt.Errorf("来源 %q 是内核保留名，扩展不能发这类事件", p.Source)
	case !s.events[[2]string{p.Source, p.Event}]:
		return tr, fmt.Errorf("未声明的来源或事件 %q/%q", p.Source, p.Event)
	case len(p.Threads) == 0:
		return tr, fmt.Errorf("事件缺少主对象")
	case p.Addressed != nil && *p.Addressed != "direct" && *p.Addressed != "mention":
		return tr, fmt.Errorf("addressed 无效")
	}
	at, err := engine.IsoTime(p.At)
	if err != nil {
		return tr, err
	}
	tr = engine.Trigger{ID: p.ID, Source: p.Source, Event: p.Event, At: at, Human: p.Human, Text: p.Text, Meta: p.Meta,
		Actor: engine.ActorRef{Source: p.Actor.Source, ID: p.Actor.ID}}
	for _, ref := range p.Threads {
		if ref.ID == "" || !s.kinds[[2]string{ref.Source, ref.Kind}] {
			return tr, fmt.Errorf("对象引用无效 %q/%q/%q", ref.Source, ref.Kind, ref.ID)
		}
		tr.Threads = append(tr.Threads, engine.NewThreadRef(ref.Source, ref.Kind, ref.ID))
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
	// 按码点定位字节边界；只在确实截断时复制调用方的元数据。
	n := 0
	for offset := range p.Text {
		if n == 4000 {
			tr.Text = p.Text[:offset]
			tr.Meta = maps.Clone(p.Meta)
			if tr.Meta == nil {
				tr.Meta = make(map[string]json.RawMessage)
			}
			tr.Meta["truncated"] = json.RawMessage("true")
			break
		}
		n++
	}
	return tr, nil
}
