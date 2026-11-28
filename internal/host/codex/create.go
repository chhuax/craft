package codex

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/chhuax/craft/internal/host"
)

var _ host.Driver = (*Driver)(nil)

// Create 先创建线程再同步登记。创建响应丢失时只能查询候选；重建次数由 launch 处理器掌管。
func (d *Driver) Create(ctx context.Context, spec host.CreateSpec) (host.Session, error) {
	if !filepath.IsAbs(spec.Directory) {
		return host.Session{}, fmt.Errorf("%w: 缺少绝对工作目录", host.Unsupported)
	}
	mode, err := sandbox(spec.Permissions)
	if err != nil {
		return host.Session{}, err
	}
	if spec.Prompt != "" || spec.DeliveryID != "" {
		if _, err := host.MarkDelivery(spec.DeliveryID, spec.Prompt); err != nil {
			return host.Session{}, err
		}
		if spec.OnCreated == nil {
			return host.Session{}, fmt.Errorf("%w: 发送首条前须登记线程", host.Unsupported)
		}
	}
	// 派工绑定只随单次 thread/start 发送，不能放在共享 app-server 的进程环境中。
	// 此时没有线程号；工作线程内由宿主提供原生线程号，CLI 可用它核对登记。
	params := map[string]any{"cwd": spec.Directory, "sandbox": mode, "approvalPolicy": "never",
		"env": host.BindingEnv(host.Session{Host: hostName}, spec.Env)}
	var r response
	if err := d.s.call(ctx, "thread/start", params, &r); err != nil {
		if errors.Is(err, host.Later) || errors.Is(err, host.Unsupported) {
			return host.Session{}, err
		}
		return host.Session{}, fmt.Errorf("%w: 创建回执缺失，请在 thread/list 核对目录与标题 %q: %w", host.Unknown, spec.Title, err)
	}
	s := host.Session{Host: hostName, ID: receipt(r.Thread.ID, r.ThreadID, r.ID)}
	if s.ID == "" {
		return host.Session{}, fmt.Errorf("%w: 创建回执缺少线程 ID，请按标题 %q 核对 thread/list", host.Unknown, spec.Title)
	}
	if spec.OnCreated != nil {
		if err := spec.OnCreated(s); err != nil {
			return s, fmt.Errorf("%w: 登记线程: %w", host.Unknown, err)
		}
	}
	if spec.Title != "" {
		if err := d.s.call(ctx, "thread/name/set", map[string]string{"threadId": s.ID, "name": spec.Title}, nil); err != nil {
			return s, fmt.Errorf("%w: 线程已建，核对标题: %w", host.Unknown, err)
		}
	}
	if spec.Prompt != "" {
		r := d.Append(ctx, s, spec.Prompt, spec.DeliveryID)
		if r.Outcome != host.OK {
			return s, fmt.Errorf("发送首条: %w", errors.Join(r.Outcome, r.Err))
		}
	}
	return s, nil
}
