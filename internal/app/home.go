package app

import (
	"fmt"
	"os"
)

func resolveStateDir(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	dir, err := StateHome(getenv, userHomeDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建状态目录: %w", err)
	}
	return dir, nil
}
