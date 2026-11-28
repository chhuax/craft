# 插件包构建

在仓库根运行 `make plugin VERSION=1.2.3`，得到 `dist/plugin/`。也可指定版本和输出目录。

```sh
go run ./tools/package --version 1.2.3 --out dist/plugin
```

默认构建 darwin/arm64、darwin/amd64、linux/amd64、linux/arm64、windows/amd64。开发时可用 `--targets` 传入逗号分隔的目标列表，例如 `--targets linux/amd64,windows/amd64`。Codex 的 `commandWindows` 固定引用 `bin/craft.exe`，裁剪目录缺少该文件时会报引用错误。发布使用全部五个目标。清单、hook、共用 skill 与二进制的布局见 [ADR 0004](../../docs/adr/0004-single-universal-package.md)。

工具注入同一个版本号，固定目录与文件的修改时间，设置普通文件和入口权限，再校验 JSON、路径引用、版本和本机目标的实际输出。每个二进制超过 16 MiB 或包总量超过 80 MiB 时报告体积警告。PATH 上有 Claude Code 时在隔离配置目录运行 `claude plugin validate`，缺少该命令或本机目标时，报告会标明跳过的检查。

组装与校验均成功后才替换输出目录。分发仓库提交与打 tag 由发布流程负责。
