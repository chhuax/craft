package cred

import (
	"encoding/base64"
	"errors"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

const service = "craft"

// 探测键不含斜线，永远不会指向用户登记的条目。
const probeKey = ".availability"

var errMissing = keyring.ErrNotFound

type systemRing struct{}

func (systemRing) get(key string) (string, error) { return keyring.Get(service, key) }
func (systemRing) set(key, secret string) error {
	// macOS 后端在打开 security 子进程后才检查其 4096 字节命令缓冲。
	// 三个参数都是安全的 ASCII 标识或 Base64，不需要额外的引号。
	const command = "add-generic-password -U -s  -a  -w go-keyring-base64:\n"
	if runtime.GOOS == "darwin" && len(command)+len(service)+len(key)+base64.StdEncoding.EncodedLen(len(secret)) > 4096 {
		return keyring.ErrSetDataTooBig
	}
	return keyring.Set(service, key, secret)
}
func (systemRing) remove(key string) error { return keyring.Delete(service, key) }

type keychain struct {
	ring  backend
	index *document
}

func (k keychain) get(key string) (string, error) {
	// 只允许当前状态目录登记过的精确名称，避免原生后端大小写折叠时串用凭据。
	if _, registered := k.index.values[key]; !registered {
		return "", errMissing
	}
	value, err := k.ring.get(key)
	if errors.Is(err, errMissing) {
		return "", errMissing
	}
	if err != nil {
		return "", errors.New("系统钥匙串读取失败")
	}
	return value, nil
}

func (k keychain) set(key, secret string) error {
	// 先登记名称，意外退出最多留下 exists=false 的记录，不会留下未登记的密钥。
	if _, ok := k.index.values[key]; !ok {
		if runtime.GOOS == "windows" {
			for other := range k.index.values {
				if strings.EqualFold(other, key) {
					return errors.New("系统钥匙串已登记仅大小写不同的条目")
				}
			}
		}
		if err := k.index.set(key, ""); err != nil {
			return err
		}
	}
	if err := k.ring.set(key, secret); err != nil {
		if errors.Is(err, keyring.ErrSetDataTooBig) {
			return errors.New("凭据超过系统钥匙串的大小限制")
		}
		return errors.New("系统钥匙串写入失败")
	}
	return nil
}

func (k keychain) remove(key string) error {
	if _, registered := k.index.values[key]; !registered {
		return nil
	}
	if err := k.ring.remove(key); err != nil && !errors.Is(err, errMissing) {
		return errors.New("系统钥匙串删除失败")
	}
	return k.index.remove(key)
}
