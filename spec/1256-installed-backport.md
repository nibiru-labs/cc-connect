# 已安装源码的 runtime 最小 backport（nibiru#1256）

## 来源与部署约束
基线固定为实机 Go buildinfo 声明的 `1ffc86d604eaf48fbcb5d2b000971d2222a1092d`（vcs.modified=false），不是当前 main。main 虽有该祖先，却未保留 `platform/a2a` 和 `cmd/cc-connect/plugin_platform_a2a.go`，不可直接替换现役二进制。

七个 runtime 源文件（Codex 状态维护、只读 API、原 Unix `/runtime` 路由、测试与说明）逐字节来自 PR16 合并提交 `efad8d569ca3426b24fcdab6a3b7dbfe00f88bf6`。`core/engine.go` 保留 PR20 `9540a1b087dc19b423606ecaae11052d0ca8a197` 的 idle/unsolicited-turn 内容，只叠加 PR16 的 relay inventory 生命周期改动，并由守卫固定最终组合 blob；不以 PR16 整文件覆盖它。`core/engine_test.go` 仍逐字节匹配 PR20 blob `71de554a5860c3df1a7146c27acf7fb59227e764`。

保持整个 A2A 平台、插件注册与其余安装基线源码原字节不变；不移植其他 main 改动。守卫保留原 12 路径 allowlist、未知路径拒绝、所有 allowlist 外安装基线文件不变及 clean-tree 条件。

## 验收
- source diff 相对于安装基线只有 runtime 必要文件及本规格/构建说明；`git diff BASE -- platform/a2a cmd/cc-connect/plugin_platform_a2a.go` 为空，A2A package 与插件路径存在。
- PR16 relay 回归测试在旧 runtime 实现上先 RED（缺少 relay inventory 方法）；合并实现后 synthetic relay lifecycle/unknown/concurrent privacy 测试 GREEN。当前组合上定向 runtime/race 与 CUJ 结果记录在本文件末尾；不把之前 `8bc` source pin 的全仓结果冒作本次组合验证。
- 构建必须绑定此新 backport 精确 head，Go buildinfo 的 vcs.revision 与 modified=false 可核。目标架构由 root 在 gocc 以不含身份信息的系统元数据确认，不把未验证的 cross-build 视为可运行。
- 此分支不是当前main的普通替换发布；应针对安装基线独立审查差异，保留 PR16 main 审查历史。部署前 root 需确认 Sakura 当前回合结束及切换窗口；不可因旧 `/sessions` 连接数为零/非零猜测忙闲。

## 不做
不部署、不改凭据/ACL/config、不启新listener、不重启或中断 Sakura。源代码、候选构建与切换计划不代替生产验收。

## 本次 relay backport 验证记录

- 合成 `HandleRelay` 测试在旧 runtime 实现上编译失败，明确指出 `beginRelayRuntimeSession`、`endRelayRuntimeSession`、`setRelayRuntimeSession` 缺失（RED）；移植 PR16 runtime 实现后 `TestRuntimeEndpoint` 定向测试通过。
- Runtime 文件与 PR16 合并提交 `efad8d5` 对齐；engine 只含 PR20 源文件上的 relay lifecycle 增量，组合 blob 由 `tools/runtime_snapshot_source_guard.py` 精确固定；engine test 保持 PR20 blob。
- `go test -race ./core -run '^TestRuntimeEndpoint' -count=1` 与 `go test ./core -run '^TestCUJ' -count=1` 均通过；`gofmt -d`、`git diff --check` 和 guard 语法检查通过。
- Guard clean positive 报告仅有原 12 个 allowlist 路径、其余安装基线文件 unchanged。临时负例分别把 engine 恢复为旧 PR20-only blob `ed5cbe10`（被组合 blob pin 拒绝）及提交一个 allowlist 外文件（被拒绝）；负例 worktree 已移除。
- 此分支仍是离线源码，不是已部署或生产验收结果；未运行默认 web build 或 Darwin 原生构建。

## 既有 PR17 基线验证记录（不覆盖本次 relay 增量）

- 先写规格及 PR16 原测试，目标运行因缺少 RuntimeSnapshot/handler 编译失败（RED）；定向移植后 Runtime race 通过（GREEN）。
- 完整 `go test -race ./core ./agent/codex ./platform/a2a`：core 50.947s / Codex 4.747s / A2A 1.580s；不是按名字过滤后空跑 A2A。
- CUJ 2.959s；`go vet ./core ./agent/codex ./platform/a2a`、`go build ./...`、`git diff --check` 通过。`go list -deps -tags no_web ./cmd/cc-connect` 同时含 A2A 与 Codex。
- 独立只读审查确认：runtime 七文件与 PR16 字节一致；A2A、Nibiru lifecycle、config、cron、CLI 和依赖文件相对安装基线原样。来源守卫在干净提交后生成可复核清单；构建候选不是已安装或验收通过的运行服务。
- 首轮全仓仅 Feishu `TestLark_GroupReplyAllWithThreadIsolationUsesRootSessionKeyWithoutMention` 两秒超时；独立安装基线 worktree 同用例亦失败，而 backport 单独复验通过（1.349s），证实不是本补丁新增确定回归。未修改该测试或 Feishu 代码；保留原失败证据并复跑全仓。
- 在不改 Feishu 的情况下重跑全仓 `umask 022; go test ./...` 已成功（exit 0）；原偶发与安装基线反例记录保留，不隐去首轮失败。
