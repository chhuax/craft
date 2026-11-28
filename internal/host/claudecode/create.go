package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/host"
)

var _ host.Driver = (*Driver)(nil)

// Capabilities 声明建会话与派工首条分开，忙时可向本实例持有的流追加。
func (*Driver) Capabilities() host.Capabilities {
	return host.Capabilities{CreateSeparatesPrompt: true, BusyAppend: true, StopHook: true, SessionStatus: true, LastReply: true}
}

var backgroundID = regexp.MustCompile(`(?m)^\s*backgrounded\s*[·:\-]\s*([0-9a-fA-F]{6,32})(?:\s|$)`)

func (d *Driver) createdID(ctx context.Context, out string) (string, error) {
	var r struct {
		SessionID         string `json:"sessionId"`
		SessionSnake      string `json:"session_id"`
		UUID, ID, ShortID string
	}
	short := ""
	if json.Unmarshal([]byte(out), &r) == nil {
		for _, id := range []string{r.SessionID, r.SessionSnake, r.UUID} {
			if uuid.MatchString(id) {
				return id, nil
			}
		}
		short = r.ID
		if short == "" {
			short = r.ShortID
		}
	} else if m := backgroundID.FindStringSubmatch(out); len(m) > 0 {
		short = m[1]
	}
	if !shortID.MatchString(short) {
		return "", fmt.Errorf("%w: 创建回执缺少会话号，请按标题核对 agents 清单", host.Unknown)
	}
	list, err := d.list(ctx, true)
	if err != nil {
		return "", fmt.Errorf("%w: 对账后台短号 %s: %w", host.Unknown, short, err)
	}
	id := ""
	for _, a := range list {
		if a.ID == short {
			if id != "" {
				return "", fmt.Errorf("%w: 后台短号对应多个会话", host.Unknown)
			}
			id = a.SessionID
		}
	}
	if id == "" {
		return "", fmt.Errorf("%w: 清单中没有后台短号 %s", host.Unknown, short)
	}
	return id, nil
}

// Create 只让 --bg 建立后台会话。业务正文必须在完整 UUID 登记成功后通过 stdin 发送。
// 登记拒绝或后续追加失败时仍返回已知的 Session，便于调用方停止或人工核对。
func (d *Driver) Create(ctx context.Context, spec host.CreateSpec) (host.Session, error) {
	if !filepath.IsAbs(spec.Directory) || strings.HasPrefix(spec.Title, "-") || strings.ContainsAny(spec.Title, "\x00\r\n") {
		return host.Session{}, fmt.Errorf("%w: 工作目录或标题无效", host.Unsupported)
	}
	if spec.Prompt != "" || spec.DeliveryID != "" {
		if _, err := host.MarkDelivery(spec.DeliveryID, spec.Prompt); err != nil {
			return host.Session{}, err
		}
		if spec.OnCreated == nil {
			return host.Session{}, fmt.Errorf("%w: 发送首条前须登记会话", host.Unsupported)
		}
	}
	perms, err := permissionArgs(spec.Permissions)
	if err != nil {
		return host.Session{}, err
	}
	title := spec.Title
	if title == "" {
		title = "worker"
	}
	args := append([]string{"--bg", "--name", title}, perms...)
	args = append(args, "--output-format", "json")
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(d.life, cancel)
	defer stop()
	defer cancel()
	if err := d.life.Err(); err != nil {
		return host.Session{}, err
	}
	binding := host.BindingEnv(host.Session{Host: hostName}, spec.Env)
	out, err := host.LaunchBackground(ctx, d.program, spec.Directory, args, binding, 30*time.Second)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return host.Session{}, fmt.Errorf("启动 Claude Code: %w", err)
		}
		return host.Session{}, fmt.Errorf("%w: 请按标题 %q 核对后台清单: %w", host.Unknown, title, err)
	}
	id, err := d.createdID(ctx, out)
	if err != nil {
		return host.Session{}, err
	}
	s := host.Session{Host: hostName, ID: id}
	if spec.OnCreated != nil {
		if err := spec.OnCreated(s); err != nil {
			return s, fmt.Errorf("%w: 登记会话: %w", host.Unknown, err)
		}
	}
	if d.lookup == nil {
		// 当前实例可沿用创建时的绑定；跨代或重启须由 lookup 提供当前派工的上下文。
		perms := spec.Permissions
		perms.Tools = slices.Clone(perms.Tools)
		d.mu.Lock()
		d.specs[id] = host.CreateSpec{Directory: spec.Directory, Permissions: perms, Env: binding}
		d.mu.Unlock()
	}
	if spec.Prompt != "" {
		r := d.Append(ctx, s, spec.Prompt, spec.DeliveryID)
		if r.Outcome != host.OK {
			return s, fmt.Errorf("发送首条: %w", errors.Join(r.Outcome, r.Err))
		}
	}
	return s, nil
}
