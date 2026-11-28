package app

import (
	"fmt"
	"os"
	"path/filepath"
)

func resolveStateDir(getenv func(string) string, userHomeDir func() (string, error)) (string, error) {
	dir := getenv("CRAFT_HOME")
	if dir == "" {
		home, err := userHomeDir()
		if err != nil {
			return "", fmt.Errorf("查找用户目录: %w", err)
		}
		dir = filepath.Join(home, ".craft")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("解析状态目录: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("创建状态目录: %w", err)
	}
	return dir, nil
}
