package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
)

// 会话号返回前可传宿主、角色、Work 与派工。EnvSessionID 只在宿主已确认 ID 后传给后续命令。
// 环境来自调用方，不能充当人的授权；CLI 仍须核对本地派工与宿主会话。
const (
	EnvHost       = "CRAFT_SESSION_HOST"
	EnvSessionID  = "CRAFT_SESSION_ID"
	EnvAssignment = "CRAFT_ASSIGNMENT_ID"
	EnvWork       = "CRAFT_WORK_ID"
	EnvRole       = "CRAFT_WATCH_ROLE"
	// EnvCodexThreadID 由宿主在取得线程号后设置，绝不能从 daemon 继承。
	EnvCodexThreadID = "CODEX_THREAD_ID"
)

// BindingEnv 只接受派工方给出的归属，忽略输入中自报的宿主、角色和会话号。
// ID 为空时必须省略变量，避免新会话继承 daemon 自己的身份。
func BindingEnv(s Session, env map[string]string) map[string]string {
	binding := map[string]string{EnvHost: s.Host, EnvRole: "worker"}
	if s.ID != "" {
		binding[EnvSessionID] = s.ID
	}
	for _, k := range []string{EnvWork, EnvAssignment} {
		if v := env[k]; v != "" {
			binding[k] = v
		}
	}
	return binding
}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func siteEnvironment() ([]string, error) {
	dir := os.Getenv("CRAFT_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("定位宿主配置: %w", err)
		}
		dir = filepath.Join(home, ".craft")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "host.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 host.json: %w", err)
	}
	var config struct {
		Names []string `json:"envPassthrough"`
	}
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("解析 host.json: %w", err)
	}
	for _, key := range config.Names {
		if !variableName.MatchString(key) {
			// 无效项可能包含用户误填的凭据，不能把值带进诊断。
			return nil, errors.New("host.json 的 envPassthrough 只接受环境变量名")
		}
	}
	return config.Names, nil
}

// USER、LOGNAME、USERNAME 必须保留：Claude Code 在 macOS 上按当前用户名读钥匙串里的登录凭据，
// 缺了就报 "Not logged in"（实测，2.1.x）。它们只是账户名，不含凭据。
var baseEnvironment = []string{
	"PATH", "HOME", "USER", "LOGNAME", "USERNAME", "USERPROFILE", "SystemRoot", "LOCALAPPDATA", "APPDATA", "TMPDIR", "TEMP", "TMP", "LANG", "CRAFT_HOME",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
}

func envKey(s string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(s)
	}
	return s
}

// 前缀由驱动声明，站点只能逐项加入名称。绑定只从本次派工读取，不能从 daemon 借用。
func commandEnv(binding map[string]string, prefixes []string) ([]string, error) {
	var site []string
	if len(prefixes) > 0 {
		var err error
		site, err = siteEnvironment()
		if err != nil {
			return nil, fmt.Errorf("%w: %w", Unsupported, err)
		}
	}
	names := make(map[string]bool, len(baseEnvironment)+len(site))
	for _, name := range baseEnvironment {
		names[envKey(name)] = true
	}
	for _, name := range site {
		names[envKey(name)] = true
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = envKey(key)
		switch key {
		case EnvHost, EnvSessionID, EnvAssignment, EnvWork, EnvRole, EnvCodexThreadID:
			continue
		}
		if names[key] || slices.ContainsFunc(prefixes, func(prefix string) bool { return strings.HasPrefix(key, prefix) }) {
			env = append(env, entry)
		}
	}
	for _, key := range []string{EnvHost, EnvRole, EnvSessionID, EnvWork, EnvAssignment} {
		if value := binding[key]; value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env, nil
}
