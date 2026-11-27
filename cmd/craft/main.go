package main

import (
	"os"

	"github.com/chhuax/craft/internal/cli"
)

// 发布时用 -ldflags 注入与归档一致的版本号。
var version = "dev"

func main() {
	os.Exit(cli.Run(os.Args[1:], version, os.Stdout, os.Stderr))
}
