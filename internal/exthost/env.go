package exthost

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/chhuax/craft/pkg/protocol"
)

// CredentialDecl 只描述本扩展名下的一个凭据和子进程读取它的变量名。
type CredentialDecl struct {
	Name string `json:"name"`
	Env  string `json:"env"`
}

var variableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func envKey(name string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(name)
	}
	return name
}

func (m *Manifest) validateEnvironment() error {
	if len(m.Credentials) > 0 && !protocol.ValidCredentialName(m.ID) {
		return errors.New("声明 credentials 的扩展 id 须为有效 provider")
	}
	passed := map[string]bool{}
	for _, name := range m.Env {
		key := envKey(name)
		if !variableName.MatchString(name) || passed[key] {
			return errors.New("env 变量名无效或重复")
		}
		passed[key] = true
	}
	names, targets := map[string]bool{}, map[string]bool{}
	for _, c := range m.Credentials {
		key := envKey(c.Env)
		if !protocol.ValidCredentialName(c.Name) || !variableName.MatchString(c.Env) {
			return errors.New("credentials 名称或目标变量无效")
		}
		if names[c.Name] || targets[key] {
			return errors.New("credentials 名称或目标变量重复")
		}
		names[c.Name], targets[key] = true, true
	}
	return nil
}

var baseEnvironment = []string{
	"PATH", "HOME", "USER", "LOGNAME", "USERNAME", "USERPROFILE", "SystemRoot", "LOCALAPPDATA", "APPDATA", "TMPDIR", "TEMP", "TMP", "LANG", "CRAFT_HOME",
	"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
}

// CredentialGetter 只查询当前扩展和声明名；第二个返回值区分缺失与读取故障。
type CredentialGetter func(provider, name string) (string, bool, error)

func (m Manifest) environment(get CredentialGetter) ([]string, string, error) {
	env := map[string]string{}
	for _, name := range slices.Concat(baseEnvironment, m.Env) {
		if value, ok := os.LookupEnv(name); ok {
			env[envKey(name)] = name + "=" + value
		}
	}
	var missing []string
	for _, c := range m.Credentials {
		var secret string
		var exists bool
		var err error
		if get != nil {
			secret, exists, err = get(m.ID, c.Name)
		}
		if err != nil {
			return nil, "", fmt.Errorf("无法读取扩展凭据 %s/%s", m.ID, c.Name)
		}
		key := envKey(c.Env)
		if !exists {
			// 声明了保险库归属的变量不能在退出登录后退回 daemon 的同名密钥。
			delete(env, key)
			missing = append(missing, m.ID+"/"+c.Name)
			continue
		}
		env[key] = c.Env + "=" + secret
	}
	// 空切片也必须非 nil，否则 os/exec 会重新继承整个环境。
	out := make([]string, 0, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		out = append(out, env[key])
	}
	warning := ""
	if len(missing) > 0 {
		warning = "缺少凭据 " + strings.Join(missing, "、")
	}
	return out, warning, nil
}
