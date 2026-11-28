package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/chhuax/craft/internal/app"
	"github.com/chhuax/craft/internal/conformance"
	"github.com/chhuax/craft/internal/engine"
	"github.com/chhuax/craft/pkg/protocol"
)

type role int

const (
	local role = iota
	anySession
	reader
	person
	worker
	settings
)

type commandSpec struct {
	path, positional, options string
	role                      role
	handle                    func(*invocation) (result, error)
}

type result struct {
	payload  any
	evidence []any
}

// CommandContext 是进程入口提供的可信上下文。扩展目录由安装方指定，普通参数不能声明身份或对象能力。
type CommandContext struct {
	Session       *app.SessionBinding
	ExtensionDirs []string
}

type invocation struct {
	stderr        io.Writer
	extensionDirs []string
	ctx           context.Context
	spec          commandSpec
	args          []string
	flags         map[string]string
	home          string
	binding       *app.SessionBinding
	clock         engine.Clock
	version       string
	getenv        func(string) string
	userHomeDir   func() (string, error)
}

func commands() []commandSpec {
	return []commandSpec{
		{"version", "", "", local, versionResult},
		{"doctor", "", "", local, doctorResult},
		{"hook session-start", "", "--host HOST", local, sessionStart},
		{"daemon run", "", "", person, daemonRun},
		{"daemon start", "", "", person, daemonStart},
		{"daemon stop", "", "[--force]", person, daemonStop},
		{"daemon status", "", "", reader, daemonStatus},
		{"act", "<action>", "--params JSON [--yes] [--key KEY] [--target NAME]", anySession, outboundCommand},
		{"outbox list", "", "", reader, daemonRPC},
		{"outbox show", "<id>", "", reader, daemonRPC},
		{"outbox confirm", "<id>", "[--yes] [--target NAME]", person, outboundCommand},
		{"outbox withdraw", "<id>", "", person, outboundCommand},
		{"alarm list", "", "", reader, daemonRPC},
		{"alarm run", "<ext> <alarm>", "[--wait [SEC]]", person, alarmRun},
		{"ext enable", "<id>", "", settings, daemonRPC},
		{"ext disable", "<id>", "", settings, daemonRPC},
		{"ext test", "<path>", "[--fixtures DIR]", person, extTest},
		{"ext list", "", "", reader, extList},
		{"ext logs", "<id>", "[--tail N]", reader, extLogs},
		{"--help", "", "", local, helpResult},
		{"watch status", "", "", reader, status},
		{"watch attention", "", "[--cursor C]", reader, attention},
		{"watch list", "", "[--open] [--thread K] [--cursor C]", reader, listWorks},
		{"watch read", "<work>", "[--offset N]", worker, readWork},
		{"watch events", "", "[--since REV]", reader, events},
		{"watch decisions", "", "[--kind K] [--source S] [--would-accept] [--limit N] [--cursor C]", reader, decisions},
		{"watch request", "<thread-ref>", "[--text T]", person, request},
		{"watch stop", "<work>", "", person, stop},
		{"watch retry", "<work>", "", person, retry},
		{"watch answer", "<work>", "--text T", person, answer},
		{"watch reevaluate", "<trigger>", "", person, reevaluate},
		{"watch attach", "<work> <trigger>", "", person, attach},
		{"watch unsubscribe", "<thread-key>", "", person, unsubscribe},
		{"watch dismiss", "<decision>", "", person, dismiss},
		{"watch turn-ended", "", "", worker, turnEnded},
		{"watch report", "<state>", "--summary T [--question Q]", worker, report},
		{"watch handback", "<trigger>", "--reason T", worker, handback},
		{"watch note", "", "--text T", worker, note},
		{"watch follow-context", "<thread-ref>", "", worker, followContext},
		{"watch mark-unrelated", "<thread-key>", "", worker, markUnrelated},
		{"policy show", "", "", reader, showPolicy},
		{"policy set", "<settings-json>", "", settings, setPolicy},
	}
}

func helpResult(*invocation) (result, error) {
	var b strings.Builder
	b.WriteString("用法")
	for _, c := range commands() {
		fmt.Fprintf(&b, "\n  craft %s", c.path)
		if c.positional != "" {
			fmt.Fprintf(&b, " %s", c.positional)
		}
		if c.options != "" {
			fmt.Fprintf(&b, " %s", c.options)
		}
		b.WriteString(" [--json]")
		switch c.path {
		case "version":
			b.WriteString("    显示构建版本")
		case "doctor":
			b.WriteString("    运行安装自检")
		case "--help":
			b.WriteString("    显示帮助")
		case "policy set":
			b.WriteString("    按 JSON 中给出的字段更新设置")
		case "watch note":
			b.WriteString("    当前引擎尚无写备注命令")
		}
	}
	return data(map[string]string{"help": b.String()})
}
func versionResult(i *invocation) (result, error) {
	return data(map[string]string{"version": i.version})
}
func doctorResult(i *invocation) (result, error) {
	return data(app.Doctor(i.ctx, i.getenv, i.userHomeDir))
}
func extTest(i *invocation) (result, error) {
	checks := conformance.Run(i.ctx, i.args[0], conformance.Options{Fixtures: i.flags["--fixtures"], KernelVersion: i.version})
	return data(checks)
}

func data(v any) (result, error) { return result{payload: v}, nil }

var errArguments = errors.New("参数错误")
var errDenied = errors.New("角色被拒绝")
var errDependency = errors.New("依赖不可用")

func invalid(format string, values ...any) error {
	return fmt.Errorf("%w: %s", errArguments, fmt.Sprintf(format, values...))
}

// 选项清单同时用于帮助和解析，方括号标记可选项，大写占位符标记取值选项。
func (i *invocation) parse(args []string) error {
	type option struct{ required, value bool }
	known := map[string]option{}
	words := strings.Fields(i.spec.options)
	for n := 0; n < len(words); n++ {
		word := words[n]
		o := option{required: !strings.HasPrefix(word, "[")}
		name := strings.Trim(word, "[]")
		if n+1 < len(words) && !strings.Contains(words[n+1], "--") {
			o.value = true
			n++
		}
		known[name] = o
	}
	i.flags = map[string]string{}
	for n := 0; n < len(args); n++ {
		token := args[n]
		if !strings.HasPrefix(token, "-") {
			if token == "" {
				return invalid("位置参数不能为空")
			}
			i.args = append(i.args, token)
			continue
		}
		name, v, inline := strings.Cut(token, "=")
		o, ok := known[name]
		_, seen := i.flags[name]
		if !ok || seen {
			return invalid("未知或重复的选项 %q", name)
		}
		if o.value {
			if !inline && name == "--wait" && (n+1 == len(args) || strings.HasPrefix(args[n+1], "--")) {
				v = "30"
			} else if !inline {
				if n+1 == len(args) || strings.HasPrefix(args[n+1], "--") {
					return invalid("%s 缺少值", name)
				}
				n++
				v = args[n]
			}
			if strings.TrimSpace(v) == "" {
				return invalid("%s 的值为空", name)
			}
		} else if inline {
			return invalid("%s 不接受值", name)
		}
		i.flags[name] = v
	}
	if len(i.args) != len(strings.Fields(i.spec.positional)) {
		return invalid("用法 craft %s %s %s", i.spec.path, i.spec.positional, i.spec.options)
	}
	for name, o := range known {
		if _, ok := i.flags[name]; o.required && !ok {
			return invalid("需要 %s", name)
		}
	}
	return nil
}

func findCommand(args []string) (commandSpec, []string, error) {
	for _, c := range commands() {
		path := strings.Fields(c.path)
		if len(args) >= len(path) && slices.Equal(args[:len(path)], path) {
			return c, args[len(path):], nil
		}
	}
	return commandSpec{}, nil, invalid("请运行 craft --help 查看用法")
}

// Run 返回进程退出码，使宿主和终端能区分参数错误、拒绝与存储故障。
func Run(args []string, version string, stdout, stderr io.Writer) int {
	return run(args, version, stdout, stderr, os.Getenv, os.UserHomeDir)
}

func run(args []string, version string, stdout, stderr io.Writer, getenv func(string) string, userHomeDir func() (string, error)) int {
	return runBound(args, version, stdout, stderr, getenv, userHomeDir, CommandContext{}, nowUTC)
}

// RunWithContext 接受进程入口提供的会话身份或派工声明，普通参数不能声明角色。
func RunWithContext(args []string, version string, stdout, stderr io.Writer, trusted CommandContext) int {
	return runBound(args, version, stdout, stderr, os.Getenv, os.UserHomeDir, trusted, nowUTC)
}

func nowUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000Z") }

func (i *invocation) prepare() error {
	var err error
	if i.binding.NeedsResolution() {
		i.home, err = app.StateHome(i.getenv, i.userHomeDir)
		if err != nil {
			return err
		}
		i.binding, err = app.BindingForSession(i.ctx, i.home, i.binding.Session)
		if err != nil {
			return err
		}
	}
	switch {
	case i.spec.role == worker && (i.binding == nil || i.binding.Session.Host == "" || i.binding.Session.ID == "" || i.binding.Assignment == ""):
		return fmt.Errorf("%w: 需要宿主提供当前 worker 会话及派工绑定", errDenied)
	case i.spec.role != worker && i.spec.role != anySession && i.binding != nil:
		return fmt.Errorf("%w: 执行方只能操作自己的 Work", errDenied)
	case i.spec.path == "ext test":
		// 一致性测试在自己的临时状态中运行，角色检查仍然适用。
		return nil
	}
	if i.home == "" {
		i.home, err = app.StateHome(i.getenv, i.userHomeDir)
		if err != nil {
			return err
		}
	}
	group, _, _ := strings.Cut(i.spec.path, " ")
	// watch request 和 attach 也要读取扩展声明，以核对对象类型是否可关闭。
	if i.extensionDirs == nil && (group == "daemon" || group == "ext" || i.spec.path == "watch request" || i.spec.path == "watch attach") {
		i.extensionDirs = app.ExtensionDirs(i.home, i.getenv("CRAFT_EXTENSIONS"))
	}
	return nil
}

func runBound(args []string, version string, stdout, stderr io.Writer, getenv func(string) string, userHomeDir func() (string, error), trusted CommandContext, clock engine.Clock) int {
	asJSON := slices.Contains(args, "--json")
	if asJSON {
		args = slices.DeleteFunc(slices.Clone(args), func(s string) bool { return s == "--json" })
	}
	spec, tail, err := findCommand(args)
	i := invocation{stderr: stderr, ctx: context.Background(), spec: spec, binding: trusted.Session, extensionDirs: trusted.ExtensionDirs, clock: clock, version: version, getenv: getenv, userHomeDir: userHomeDir}
	if err == nil {
		err = i.parse(tail)
	}
	if err == nil && spec.role != local {
		err = i.prepare()
	}
	if err != nil {
		return failure(stderr, asJSON, err)
	}

	res, err := spec.handle(&i)
	if err != nil {
		return failure(stderr, asJSON, err)
	}
	switch checks := res.payload.(type) {
	case sessionStartOutput:
		// 宿主只识别自己的 JSON 信封，输出失败也不能中断会话启动。
		if err := json.NewEncoder(stdout).Encode(checks); err != nil {
			_, _ = fmt.Fprintf(stderr, "写入 SessionStart 输出失败: %v\n", err)
		}
		return 0
	case []app.Check:
		return reportChecks(stdout, asJSON, checks, "failed", "安装自检未通过")
	case []conformance.Check:
		rows := make([]app.Check, len(checks))
		for i, c := range checks {
			rows[i] = app.Check(c)
		}
		return reportChecks(stdout, asJSON, rows, "fail", "扩展协议一致性测试未通过")
	}
	if err := render(stdout, res, asJSON); err != nil {
		return failure(stderr, asJSON, err)
	}
	return 0
}

func failure(w io.Writer, asJSON bool, err error) int {
	code, exit := rpcFailure(err)
	detail := map[string]string{}
	message := err.Error()
	if e, ok := errors.AsType[*engine.UserError](err); ok {
		code, exit = "refused", 5
		// 引擎的拒绝是给人看的结论，只显示它自己的话；事务与命令层层包装出来的前缀对用户是噪声，
		// 但排查时有用，所以放进 detail.chain 而不是丢掉。
		message = e.Error()
		detail["chain"] = clipText(err.Error())
		if engine.IsMissing(err) {
			code = "missing"
		}
		for key, value := range e.Values {
			detail[key] = clipText(value)
		}
		detail["code"] = e.Code
	} else {
		switch {
		case errors.Is(err, errArguments), errors.Is(err, app.ErrInvalidSettings):
			code, exit = "invalidArguments", 2
		case errors.Is(err, errDenied), errors.Is(err, app.ErrOtherWork):
			code, exit = "denied", 4
		case errors.Is(err, errMissing):
			code, exit = "missing", 5
		case errors.Is(err, errOwned), errors.Is(err, errDependency), errors.Is(err, app.ErrObjectKinds):
			code, exit = "unavailable", 6
		}
	}
	var publicDetail any = detail
	message = clipText(message)
	if remote, ok := errors.AsType[*protocol.Error](err); ok && remote.Data != nil {
		publicDetail = *remote.Data
		if !asJSON {
			message += "\n" + string(*remote.Data)
		}
	}
	if asJSON {
		err = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message, "detail": publicDetail}})
	} else {
		_, err = fmt.Fprintln(w, message)
	}
	if err != nil {
		return 1
	}
	return exit
}
func reportChecks(stdout io.Writer, asJSON bool, checks []app.Check, failed, message string) int {
	ok := !slices.ContainsFunc(checks, func(c app.Check) bool { return c.Status == failed })
	if asJSON {
		envelope := map[string]any{"ok": ok, "payload": map[string][]app.Check{"checks": checks}, "evidence": []string{}}
		// 失败时也保留逐项报告，兼容已经消费 doctor 检查项的调用方。
		if !ok {
			envelope["error"] = map[string]any{"code": "checkFailed", "message": message, "detail": map[string]string{}}
		}
		if err := json.NewEncoder(stdout).Encode(envelope); err != nil {
			return 1
		}
	} else {
		for _, c := range checks {
			// 路径和系统错误可能包含换行，引用说明可让每项始终只占一行。
			if _, err := fmt.Fprintf(stdout, "%s\t%s\t%q\n", c.Name, c.Status, c.Detail); err != nil {
				return 1
			}
		}
	}
	if !ok {
		return 1
	}
	return 0
}
