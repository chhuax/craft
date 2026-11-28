package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/chhuax/craft/internal/alarm"
	"github.com/chhuax/craft/internal/exthost"
	"github.com/chhuax/craft/internal/store"
)

// Stale 比对本次装载与当前 manifest 元数据；覆盖中的不完整指纹不能触发重载。
func (d *Daemon) Stale() bool { return d.fingerprintChanged(d.initialFingerprint) }

// View 使用 daemon 的只读连接池，所有分页字段共享一个 WAL 快照。
func (d *Daemon) View(ctx context.Context, fn func(CommandReader) error) error {
	return d.db.View(ctx, func(rd store.SQLiteReader) error { return fn(CommandReader{&rd}) })
}

// Revision 读取与事件追加同时提交的游标；尚无事件时为零。
func (r CommandReader) Revision() (int64, error) {
	if r.read == nil {
		return 0, nil
	}
	raw, found, err := r.read.Get("watch_kv", "event.rev")
	if err != nil || !found {
		return 0, err
	}
	var rev int64
	if err := json.Unmarshal(raw, &rev); err != nil {
		return 0, fmt.Errorf("读取事件修订号: %w", err)
	}
	return rev, nil
}

// Alarms 在 daemon 就绪后返回调度器快照。
func (d *Daemon) Alarms() []alarm.Snapshot { return d.alarms.List() }

// ErrAlarmUnavailable 表示扩展在当前装载周期内未就绪。
var ErrAlarmUnavailable = errors.New("扩展未就绪")

// ErrUnknownAlarm 表示扩展没有登记该闹钟。
var ErrUnknownAlarm = errors.New("找不到闹钟")

// QueueAlarm 按已登记的扩展与闹钟标识持久化请求。
func (d *Daemon) QueueAlarm(ctx context.Context, ext, id string) (string, error) {
	if !d.knownExtensions[ext] {
		return "", ErrUnknownExtension
	}
	if d.Health().Extensions[ext].Status != "ready" {
		return "", ErrAlarmUnavailable
	}
	// 登记表在发布 ready 之前建好，运行期间不会变更。
	if !d.queue.registered[ext+":"+id] {
		return "", ErrUnknownAlarm
	}
	return EnqueueAlarm(ctx, d.db, ext, id)
}

// AlarmRequest 从同一只读快照取请求状态，等待超时不会影响执行。
func (r CommandReader) AlarmRequest(id string) (AlarmRequest, error) {
	if r.read == nil {
		return AlarmRequest{}, fmt.Errorf("闹钟请求不存在")
	}
	return alarmRequest(*r.read, id)
}

// ErrUnknownExtension 表示标识未通过本次装载时的 manifest 校验。
var ErrUnknownExtension = errors.New("找不到扩展")

// SetExtensionEnabled 保存装载选择；拓扑排序、事件目录与闹钟须在同一装载周期内切换。
func (d *Daemon) SetExtensionEnabled(ctx context.Context, id string, enabled bool) error {
	if !d.knownExtensions[id] {
		return ErrUnknownExtension
	}
	return d.db.Write(ctx, func(tx *store.SQLiteTx) error {
		key := "extension.disabled:" + id
		if enabled {
			return tx.Delete("watch_kv", key)
		}
		return tx.Put("watch_kv", key, []byte("true"))
	})
}

func (d *Daemon) selected(ctx context.Context, exts []exthost.Extension) ([]exthost.Extension, error) {
	d.knownExtensions = make(map[string]bool, len(exts))
	var selected []exthost.Extension
	err := d.db.View(ctx, func(r store.SQLiteReader) error {
		for _, e := range exts {
			id := e.Manifest.ID
			d.knownExtensions[id] = true
			_, disabled, err := r.Get("watch_kv", "extension.disabled:"+id)
			if err != nil {
				return err
			}
			if disabled {
				d.health.set(d.health.state.Extensions, id, "disabled", "")
				continue
			}
			selected = append(selected, e)
		}
		return nil
	})
	return selected, err
}
