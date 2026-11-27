# 宿主包构建

本目录将在宿主适配就绪后承担四项工作。

1. 按目标系统和架构编译 CLI，并注入发布版本。
2. 按「宿主 × 系统」渲染 manifest 中的 hook 和命令入口模板。
3. 将宿主文件、对应的二进制、`VERSION`、`LICENSE` 和 `NOTICE` 归档。Windows 使用 ZIP，macOS 和 Linux 使用 tar.gz。
4. 为归档生成 `checksums.txt`，供下载后校验。

包名与内容布局见 [ADR 0004](../../docs/adr/0004-per-host-per-os-packages.md)。
