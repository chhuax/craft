# 把验证绑定到实际代码

任务完成后跑窄验证，全部提交后用 `craft method record --run "go test ./catalog/..."` 运行改动模块的全部测试，将示例命令换成项目的实际命令。手工检查用 `craft method record --cmd "检查空列表导出" --exit 0` 记录观察到的结果码。全量回归单独说明覆盖范围。命令没有跑完、执行期间内容改变或没有前后快照，都无法证明当前代码通过。

独立读者拿需求、方案、代码差异和收尾记录，按每个 S-n 分别判断。只有收到带证据位置的报告才登记。无法独立复核时如实交代本次验证者与限制。样例报告如下。

```json
{"version":1,"by":"复核人","scenarios":[{"id":"S-1","result":"pass","evidence":"TestFilterPrefix"},{"id":"S-2","result":"unverified","evidence":"缺少只读用户的验收环境"}]}
```

用 `craft method verify --by "复核人" --report FILE` 保存真实报告。允许 pass、fail、unverified，每个场景恰有一条。有未证实项就如实交付；A-n 单列为待人确认。报告放在状态目录或项目外，也可从标准输入传入。写入项目内可见的新文件会改变快照。

`craft method evidence` 给出确认、收尾和独立验证各自的状态。代码或 HEAD 已变时重新收尾并复核。页面场景分别给出算法、组件、浏览器实际操作及用户本人验收的证据，只拿到截图时说清尚未验证的交互。
