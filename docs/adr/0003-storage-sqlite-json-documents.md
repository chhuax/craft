# ADR 0003：单个 SQLite 文件，聚合存为 JSON 文档

状态：提议（2026-11-27）

## 决定

- 状态全部放用户目录下的一个 SQLite 文件，WAL 模式。
- Watch 的聚合（Thread、Work、Decision……）整份存为 JSON 文档，用生成列 + 部分索引支撑活跃集查询。
- 引擎代码不拼 SQL；SQL 只在 `internal/store`。
- 每条命令一个短事务（`BEGIN IMMEDIATE`），事务内重读聚合。

## 理由

- 引擎的所有命令天然是「读几个聚合、改、写回」，文档模型与它一一对应，避免为十几张规范化表写大量映射代码。
- 单文件便于备份、迁移和测试（内存仓储与 SQLite 仓储跑同一套用例）。
- 多进程写（CLI 短进程与 daemon）靠 SQLite 写锁串行；daemon 用 `PRAGMA data_version` 发现外部写入。

## 约束

- 不支持网络盘（WAL 语义不可靠）。
- 聚合体积要有上限（卡片、文本截断），避免整份读写变慢；`prune` 命令按保留期清理。
- 文档 schema 演进靠「读时容忍缺字段 + 版本号 + 迁移」，迁移脚本放 `internal/store/migrate`。
