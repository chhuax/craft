package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"time"
)

// 同一个 daemon 所有驱动共用上限，卡死的命令也占名额直到真正退出。
var processSlots = make(chan struct{}, 16)

const outputLimit = 4 << 20

var errOutputLimit = errors.New("宿主输出超过上限")

// Process 独占一棵进程树及其管道。使用者必须 Close；协议客户端还要限制每帧 stdout。
type Process struct {
	Stdin      *os.File
	Stdout     *os.File
	cancel     context.CancelCauseFunc
	done       chan struct{}
	err        error
	cleanupErr error
	stderr     string
}

// Start 启动原生可执行文件，ctx 控制整棵树的生命周期。binding 只允许覆盖会话绑定。
// 绝对路径由 daemon 选择；裸命令只从 daemon 的 PATH 解析，禁止在工作目录中找程序。
func Start(ctx context.Context, program, dir string, args []string, binding map[string]string) (*Process, error) {
	return startCommand(ctx, program, dir, args, binding, nil)
}

func startCommand(ctx context.Context, program, dir string, args []string, binding map[string]string, outputRead <-chan struct{}) (*Process, error) {
	if !filepath.IsAbs(program) && filepath.Base(program) != program {
		return nil, fmt.Errorf("宿主命令须为绝对路径或程序名")
	}
	if dir != "" && !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("宿主工作目录须为绝对路径")
	}
	cmd := exec.Command(program, args...)
	if cmd.Err != nil {
		return nil, fmt.Errorf("定位宿主命令: %w", cmd.Err)
	}
	if err := checkCommand(cmd); err != nil {
		return nil, err
	}
	cmd.Dir, cmd.Env = dir, commandEnv(binding)
	select {
	case processSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	p, err := start(ctx, cmd, outputRead)
	if err != nil {
		<-processSlots
		return nil, fmt.Errorf("启动宿主命令: %w", err)
	}
	return p, nil
}

func start(ctx context.Context, cmd *exec.Cmd, outputRead <-chan struct{}) (*Process, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var pipes [3][2]*os.File
	for i := range pipes {
		r, w, err := os.Pipe()
		if err != nil {
			for _, ends := range pipes[:i] {
				_ = ends[0].Close()
				_ = ends[1].Close()
			}
			return nil, err
		}
		pipes[i] = [2]*os.File{r, w}
	}
	in, out, diag := pipes[0], pipes[1], pipes[2]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in[0], out[1], diag[1]
	finish, err := startTree(cmd)
	_ = in[0].Close()
	_ = out[1].Close()
	_ = diag[1].Close()
	if err != nil {
		return nil, errors.Join(err, in[1].Close(), out[0].Close(), diag[0].Close())
	}

	life, cancel := context.WithCancelCause(ctx)
	p := &Process{Stdin: in[1], Stdout: out[0], done: make(chan struct{}), cancel: cancel}
	go p.supervise(life, cmd, finish, diag[0], outputRead)
	return p, nil
}

func (p *Process) supervise(ctx context.Context, cmd *exec.Cmd, finish func(bool) error, diag *os.File, outputRead <-chan struct{}) {
	// 脱离进程组的后代可能仍握着继承的管道；取消还须中止本端 IO。
	interrupted := make(chan struct{})
	unwatch := context.AfterFunc(ctx, func() {
		defer close(interrupted)
		_ = p.Stdout.Close()
		_ = p.Stdin.Close()
		_ = diag.Close()
	})
	diagDone := make(chan struct{})
	var diagErr error
	go func() {
		defer close(diagDone)
		defer func() { _ = diag.Close() }()
		p.stderr, diagErr = readBounded(diag)
		if diagErr != nil {
			p.cancel(diagErr)
		}
	}()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	var waitErr, killErr error
	select {
	case waitErr = <-exited:
		// 组长可以先于继承管道的后代退出。Wait 只等组长，回收后代后才能等读端 EOF。
		// 后台启动器成功退出且两路输出读完，才把后代交回宿主。卡住、取消或失败仍须收整树。
		handedOff := false
		if outputRead != nil && waitErr == nil {
			select {
			case <-outputRead:
				select {
				case <-diagDone:
					handedOff = ctx.Err() == nil
				case <-ctx.Done():
				}
			case <-ctx.Done():
			}
		}
		killErr = finish(!handedOff)
	case <-ctx.Done():
		killErr = finish(true)
		waitErr = <-exited
	}
	<-diagDone
	if !unwatch() {
		<-interrupted
	}
	if ctx.Err() != nil && errors.Is(diagErr, os.ErrClosed) {
		diagErr = nil
	}
	p.cleanupErr = errors.Join(killErr, diagErr)
	p.err = errors.Join(context.Cause(ctx), waitErr, p.cleanupErr)
	p.cancel(nil)
	<-processSlots
	close(p.done)
}

func readBounded(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, outputLimit+1))
	if len(data) > outputLimit {
		return "", errOutputLimit
	}
	return string(data), err
}

// Close 中止可能仍在运行的后代并等待回收。重复调用安全；已退出进程的错误由 Wait 返回。
func (p *Process) Close() error {
	p.cancel(context.Canceled)
	_ = p.Stdin.Close()
	_ = p.Stdout.Close()
	<-p.done
	return p.cleanupErr
}

// Done 在进程树和 stderr 都已回收后关闭，可与调用方的关闭期限一起等待。
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait 等到整棵树退出。流式调用者应先消费 stdout，或调用 Close 主动结束。
func (p *Process) Wait() error {
	<-p.done
	return p.err
}

// Run 把输入送到 stdin 并分别限制两路输出；返回的诊断文本可能含宿主数据，不应直接记日志。
func Run(ctx context.Context, program, dir string, args []string, binding map[string]string, input string, timeout time.Duration) (string, string, error) {
	return run(ctx, program, dir, args, binding, input, timeout, nil)
}

// LaunchBackground 用于宿主自行托管的后台会话。成功退出且关闭两路输出后，由宿主的停止命令接管后代。
func LaunchBackground(ctx context.Context, program, dir string, args []string, binding map[string]string, timeout time.Duration) (string, error) {
	outputRead := make(chan struct{})
	out, _, err := run(ctx, program, dir, args, binding, "", timeout, outputRead)
	return out, err
}

func run(ctx context.Context, program, dir string, args []string, binding map[string]string, input string, timeout time.Duration, outputRead chan struct{}) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p, err := startCommand(ctx, program, dir, args, binding, outputRead)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = p.Close() }()
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(p.Stdin, strings.NewReader(input))
		written <- errors.Join(err, p.Stdin.Close())
	}()
	out, readErr := readBounded(p.Stdout)
	if readErr != nil {
		p.cancel(readErr)
	}
	if outputRead != nil {
		close(outputRead)
	}
	waitErr := p.Wait()
	if err := errors.Join(readErr, <-written, waitErr); err != nil {
		return out, p.stderr, fmt.Errorf("运行宿主命令: %w", err)
	}
	return out, p.stderr, nil
}
