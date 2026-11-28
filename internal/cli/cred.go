package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chhuax/craft/internal/cred"
	"github.com/chhuax/craft/pkg/protocol"
)

func credentialList(i *invocation) (result, error) {
	backend, entries, err := cred.New(i.home).List()
	if err != nil {
		return result{}, err
	}
	out := map[string]any{"backend": backend, "entries": entries}
	if backend == "file" {
		out["warning"] = "加密仅保护单独误传或备份的数据文件。同一用户的其他进程以及同时取得密钥文件的备份均可解密。"
	}
	return data(out)
}

func credentialChange(i *invocation) (result, error) {
	provider := i.args[0]
	name := i.flags["--name"]
	if name == "" {
		name = "default"
	}
	if !protocol.ValidCredentialName(provider) || !protocol.ValidCredentialName(name) {
		return result{}, invalid("provider 与 name 只接受 1 至 64 个 ASCII 字母、数字、点、下划线或连字符")
	}
	vault := cred.New(i.home)
	login := i.spec.path == "cred login"
	if login {
		secret, err := readSecret(os.Stdin)
		if err != nil {
			return result{}, err
		}
		if err := vault.Set(provider, name, secret); err != nil {
			return result{}, err
		}
	} else if err := vault.Delete(provider, name); err != nil {
		return result{}, err
	}
	return data(cred.Entry{Provider: provider, Name: name, Exists: login})
}

func readSecret(input *os.File) (secret string, err error) {
	restore, err := disableEcho(input)
	if err != nil {
		return "", fmt.Errorf("关闭终端回显: %w", err)
	}
	if restore != nil {
		defer func() { err = errors.Join(err, restore()) }()
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", errors.New("读取 stdin 凭据失败")
	}
	if line, ok := strings.CutSuffix(line, "\n"); ok {
		return strings.TrimSuffix(line, "\r"), nil
	}
	return line, nil
}
