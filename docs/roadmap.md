# 路线图与迁移

状态：草案。原则：先冻结规格，再写代码；增量迁移，不大爆炸。

## 里程碑

| 阶段 | 内容 | 出口 |
|---|---|---|
| M0 规格 | `docs/spec/*` 评审定稿；解决 `open-questions.md` 里阻塞项 | 规格评审通过；术语表与命名冻结 |
| M1 骨架与存储 | `go.mod`、CLI 外壳、`internal/store`（schema、文档读写、生成列、迁移）、内存仓储、`CRAFT_HOME`、`doctor` | 内存仓储与 SQLite 仓储跑同一套仓储用例 |
| M2 闹钟 | `internal/alarm`：不重叠、并发、超时、退避、立即触发、停止；注入时钟 | 拨时钟的表驱动测试全绿 |
| M3 引擎 | `internal/engine`：模型、派生函数、Intake、Evals、Works、Assignments、Replies、Policy、effect 队列、Pass；端口全用测试替身 | 行为用例全绿；`engine` 仅依赖标准库的守门测试通过 |
| M4 扩展宿主与协议 | `internal/exthost`、`pkg/protocol`、`craft ext test` 一致性套件 | 假扩展端到端（alarm → receive → 研判 → 派工 → 关闭） |
| M5 outbound | 分级、预览、`--yes`、outbox、回执、`action/verify`、内容检查 | 各级别流程测试 |
| M6 参考扩展与宿主薄包 | `extensions/github`；首发宿主薄包：Claude Code 与 Codex（skill markdown + 调用说明）；宿主能力放各自包的 manifest | 真实宿主冒烟：一条 PR 事件走完 |
| M6b method | `internal/method`（标准库，不导入内核）：模板与文档检查、确认（内容哈希）、验证证据与代码快照、任务进度（按提交识别）、变更说明投影、现状文档合并；method 执行手册与示例库 | K3：只装 craft + git，从一段需求文本走到本地提交与变更说明 |
| M7 发布 | 按 ADR 0004 产出「宿主 × 系统」包矩阵（`tools/package/`：编译、渲染入口模板、归档、校验和）；上传 GitHub Releases；CONTRIBUTING、文档站 | `v0.1.0`，10 个包与 `checksums.txt` |

## 行为用例来源

旧项目的 Watch 引擎设计参考了一个 MIT 许可的原型（约 689 个引擎用例，是行为规格）。移植用例时：

- 按模块译成 Go 表驱动测试，先全红、再实现到全绿；
- 与具体外部系统、特定宿主绑定的用例不移植；
- 移植的代码或用例文件保留原版权声明与许可文本（MIT 要求），`NOTICE` 里登记。

## 从旧项目迁移的对照

| 旧项目（Python） | 新项目 |
|---|---|
| `core/alarm/`、`core/store/alarm_sql.py` | `internal/alarm/`、`alarm_state` 表 |
| `watch/engine/`（纯领域） | `internal/engine/` |
| `watch/app/`（inbox、effects、passes、source_context） | `internal/app/` |
| `core/store/watch/` | `internal/store/` |
| `runtime/providers/`（标准协议与加载器） | `pkg/protocol/` + `internal/exthost/` |
| `runtime/outbound/` | `internal/outbound/` |
| `core/credentials`（Vault） | `internal/cred/`（内核保险库，认证由扩展登记） |
| `extensions/providers/gitlab` | 可选：开源参考扩展 `extensions/gitlab`（去公司化后） |
| `extensions/providers/{yy-pm,yy-yms,yy-yht}`、`extensions/presets/*` | **不进开源仓库**，留公司私有仓库，改写成协议扩展后接入 |
| `iuap-gp-craft`（方法论 skill 与命令组 `liuma craft`） | `internal/method/` + `skills/method/`（去企业化后，规矩 K1–K8） |
| `hosts/<宿主>/` | `hosts/<宿主>/`（薄包） |
| `workbench/`（面板） | 独立仓库，作为外部客户端；不进内核仓库 |
| `skills/`（给模型用的 skill） | 通用部分进 `hosts/_shared/skills`，公司专属部分随扩展走 |

旧项目的 `domains/`、旧 Watch 兼容模块、`links` 门面等过渡产物**不迁移**。

## 去公司化清单

开源仓库里不得出现：

- 具体外部系统名称、URL、状态码、环境标识、内部术语（工单系统、部署平台、认证网关等）；
- 公司或特定项目的业务规则（如工作小时临期升级、专属化标记、跨系统联动校验）；
- 硬编码品牌前缀与路径（`~/.liuma`、`LIUMA_*`、`IUAP_*`）；
- 测试里的真实账号、内网地址、录制数据中未脱敏的字段。

执行方式：

1. 内核与协议只用中立词汇：对象（Thread）、事件（Trigger）、工作（Work）、扩展（extension）。
2. 旧文档里的领域规则一律下沉到「扩展声明」：事件目录、动作级别、`placement_rules`、`policy_rules`。
3. CI 加一条「词表扫描」：维护一个禁用词清单，命中即失败；白名单只减不增。
4. 录制 fixture 入库前跑脱敏脚本，人工复核。
5. 迁移期间公司私有扩展与预设放在公司仓库，通过协议接入同一个二进制，验证「内核无公司知识」真的成立。

## 开源基础设施

- 许可证 Apache-2.0；`NOTICE` 登记第三方与原型的版权声明。
- 构建与发布按 ADR 0004：goreleaser 产出各系统二进制，`tools/package/` 渲染宿主模板并归档成「宿主 × 系统」包，上传 GitHub Releases。
- CI：单元测试、引擎守门（仅标准库）、词表扫描、协议一致性测试、跨平台冒烟。
- 治理：`CONTRIBUTING.md`、`CODE_OF_CONDUCT.md`、`SECURITY.md`、issue/PR 模板。
- 文档语言：规格先用中文定稿，M7 前补英文版并以英文为准对外。
