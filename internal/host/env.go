package host

import "os"

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

// commandEnv 从父进程逐项取运行必需的目录及路径；调用方不能覆盖 PATH、宿主配置或加载器。
func commandEnv(binding map[string]string) []string {
	env := make([]string, 0, 24)
	for _, key := range []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "LOCALAPPDATA", "APPDATA", "TMPDIR", "TEMP", "TMP", "LANG", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "CRAFT_HOME"} {
		if value, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+value)
		}
	}
	for _, key := range []string{EnvHost, EnvRole, EnvSessionID, EnvWork, EnvAssignment} {
		if value := binding[key]; value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}
