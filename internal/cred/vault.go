// Package cred 优先把凭据交给系统钥匙串。退化文件仅防止单独误传或备份密文时泄密；
// 密钥与数据同属一个用户，能读取整个状态目录的进程或备份也能解密。
package cred

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/chhuax/craft/pkg/protocol"
)

type backend interface {
	get(key string) (string, error)
	set(key, secret string) error
	remove(key string) error
}

// Entry 是可以展示的登记信息。用户也可能在系统钥匙串里独立删除条目。
type Entry struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
	Exists   bool   `json:"exists"`
}

// Vault 在首次使用时选择后端，同一个 daemon 的后续请求复用这次探测。
type Vault struct {
	dir  string
	ring func() backend
}

// New 绑定状态目录，尚未读写磁盘或系统钥匙串。
func New(dir string) *Vault { return newVault(dir, systemRing{}) }

func newVault(dir string, ring backend) *Vault {
	return &Vault{dir: dir, ring: sync.OnceValue(func() backend {
		if _, err := ring.get(probeKey); err != nil && !errors.Is(err, errMissing) {
			return nil
		}
		return ring
	})}
}

type entries struct {
	backend
	file *document
}

func (v *Vault) locked(f func(*entries) error) (err error) {
	if err = os.MkdirAll(v.dir, 0700); err != nil {
		return fmt.Errorf("创建保险库目录: %w", err)
	}
	// 单独的锁文件不参与 rename。关闭句柄（包括进程退出）才释放跨进程锁。
	lock, err := os.OpenFile(filepath.Join(v.dir, "credentials.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("打开保险库锁: %w", err)
	}
	defer func() { err = errors.Join(err, lock.Close()) }()
	if err = lockFile(lock); err != nil {
		return fmt.Errorf("锁定保险库: %w", err)
	}
	ring := v.ring()
	file := &document{dir: v.dir, encrypted: ring == nil}
	if err := file.load(); err != nil {
		return err
	}
	var store backend = file
	if ring != nil {
		store = keychain{ring: ring, index: file}
	}
	return f(&entries{store, file})
}

func account(provider, name string) (string, error) {
	if !protocol.ValidCredentialName(provider) || !protocol.ValidCredentialName(name) {
		return "", errors.New("无效的凭据 provider 或 name")
	}
	return provider + "/" + name, nil
}

// Get 只在条目存在且读取成功时返回凭据；缺失与保险库损坏可分别处理。
func (v *Vault) Get(provider, name string) (string, bool, error) {
	key, err := account(provider, name)
	if err != nil {
		return "", false, err
	}
	var secret string
	var exists bool
	err = v.locked(func(e *entries) error {
		var readErr error
		secret, readErr = e.get(key)
		if errors.Is(readErr, errMissing) {
			return nil
		}
		exists = readErr == nil
		return readErr
	})
	if err != nil || !exists {
		return "", false, err
	}
	return secret, true, nil
}

// Set 登记或替换凭据。钥匙串错误可能回显输入，不能直接交给调用者。
func (v *Vault) Set(provider, name, secret string) error {
	key, err := account(provider, name)
	if err != nil {
		return err
	}
	if secret == "" || strings.IndexByte(secret, 0) >= 0 || !utf8.ValidString(secret) {
		return errors.New("凭据为空或不能作为环境变量传递")
	}
	return v.locked(func(e *entries) error { return e.set(key, secret) })
}

// Delete 移除凭据及其登记；重复退出登录也能成功。
func (v *Vault) Delete(provider, name string) error {
	key, err := account(provider, name)
	if err != nil {
		return err
	}
	return v.locked(func(e *entries) error { return e.remove(key) })
}

// List 按标识排序，只返回当前后端、登记名和存在状态。
func (v *Vault) List() (string, []Entry, error) {
	mode := "file"
	rows := []Entry{}
	err := v.locked(func(e *entries) error {
		if !e.file.encrypted {
			mode = "keychain"
		}
		for _, key := range slices.Sorted(maps.Keys(e.file.values)) {
			p, n, _ := strings.Cut(key, "/")
			_, err := e.get(key)
			exists := err == nil
			if err != nil && !errors.Is(err, errMissing) {
				return err
			}
			rows = append(rows, Entry{p, n, exists})
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return mode, rows, nil
}
