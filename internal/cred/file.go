package cred

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type document struct {
	dir       string
	encrypted bool
	values    map[string]string
}

func (d *document) path() string {
	if d.encrypted {
		return filepath.Join(d.dir, "credentials.enc")
	}
	return filepath.Join(d.dir, "credentials.index")
}

func (d *document) load() error {
	d.values = map[string]string{}
	data, err := os.ReadFile(d.path())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("读取凭据文件: %w", err)
	}
	if d.encrypted {
		aead, err := d.cipher(false)
		if err != nil {
			return err
		}
		data, err = aead.Open(nil, nil, data, nil)
		if err != nil {
			return fmt.Errorf("凭据文件认证失败: %w", err)
		}
	}
	if err := json.Unmarshal(data, &d.values); err != nil || d.values == nil {
		return errors.New("凭据登记数据无效")
	}
	return nil
}

func (d *document) get(key string) (string, error) {
	value, ok := d.values[key]
	if !ok {
		return "", errMissing
	}
	return value, nil
}

func (d *document) set(key, secret string) error {
	d.values[key] = secret
	return d.save()
}

func (d *document) remove(key string) error {
	if _, ok := d.values[key]; !ok {
		return nil
	}
	delete(d.values, key)
	return d.save()
}

func (d *document) save() error {
	data, err := json.Marshal(d.values)
	if err != nil {
		return fmt.Errorf("编码凭据文件: %w", err)
	}
	if d.encrypted {
		aead, err := d.cipher(true)
		if err != nil {
			return err
		}
		data = aead.Seal(nil, nil, data, nil)
	}
	return atomicWrite(d.path(), data)
}

func (d *document) cipher(create bool) (cipher.AEAD, error) {
	path := filepath.Join(d.dir, "credentials.key")
	key, err := os.ReadFile(path)
	// 仅在持锁且已确认库可读后创建；已有密文缺少密钥时不能另造一把。
	if create && errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			err = atomicWrite(path, key)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("加载凭据文件密钥: %w", err)
	}
	if len(key) != 32 {
		return nil, errors.New("凭据文件密钥长度无效")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func atomicWrite(path string, data []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return fmt.Errorf("创建凭据临时文件: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.Remove(f.Name()))
		}
	}()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("写入凭据临时文件: %w", err)
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("替换凭据文件: %w", err)
	}
	return nil
}
