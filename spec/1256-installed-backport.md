# 已安装源码的 runtime 最小 backport（nibiru#1256）

## 来源与部署约束
基线固定为实机 Go buildinfo 声明的 `1ffc86d604eaf48fbcb5d2b000971d2222a1092d`（vcs.modified=false），不是当前 main。main 虽有该祖先，却未保留 `platform/a2a` 和 `cmd/cc-connect/plugin_platform_a2a.go`，不可直接替换现役二进制。

仅移植 PR16 `8bc232f6577a8178a42eedd51ab153c6692c20f5` 的可选只读 runtime 接口、Codex 状态维护、原 Unix `/runtime` 路由及其回归测试/说明。保持整个 A2A 平台、插件注册与其余安装基线源码原字节不变；不移植其他 main 改动。

## 验收
- source diff 相对于安装基线只有 runtime 必要文件及本规格/构建说明；`git diff BASE -- platform/a2a cmd/cc-connect/plugin_platform_a2a.go` 为空，A2A package 与插件路径存在。
- 先复用 runtime 状态/API测试看到缺接口 RED，再移植实现得到 GREEN；A2A 原测试、core/Codex、CUJ、race、vet、build 通过。全仓若有既有环境限制，记录事实。
- 构建必须绑定此新 backport 精确 head，Go buildinfo 的 vcs.revision 与 modified=false 可核。目标架构由 root 在 gocc 以不含身份信息的系统元数据确认，不把未验证的 cross-build 视为可运行。
- 此分支不是当前main的普通替换发布；应针对安装基线独立审查差异，保留 PR16 main 审查历史。部署前 root 需确认 Sakura 当前回合结束及切换窗口；不可因旧 `/sessions` 连接数为零/非零猜测忙闲。

## 不做
不部署、不改凭据/ACL/config、不启新listener、不重启或中断 Sakura。源代码、候选构建与切换计划不代替生产验收。

## 本地验证记录

- 先写规格及 PR16 原测试，目标运行因缺少 RuntimeSnapshot/handler 编译失败（RED）；定向移植后 Runtime race 通过（GREEN）。
- 完整 `go test -race ./core ./agent/codex ./platform/a2a`：core 50.947s / Codex 4.747s / A2A 1.580s；不是按名字过滤后空跑 A2A。
- CUJ 2.959s；`go vet ./core ./agent/codex ./platform/a2a`、`go build ./...`、`git diff --check` 通过。`go list -deps -tags no_web ./cmd/cc-connect` 同时含 A2A 与 Codex。
- 独立只读审查确认：runtime 七文件与 PR16 字节一致；A2A、Nibiru lifecycle、config、cron、CLI 和依赖文件相对安装基线原样。来源守卫在干净提交后生成可复核清单；构建候选不是已安装或验收通过的运行服务。
- 首轮全仓仅 Feishu `TestLark_GroupReplyAllWithThreadIsolationUsesRootSessionKeyWithoutMention` 两秒超时；独立安装基线 worktree 同用例亦失败，而 backport 单独复验通过（1.349s），证实不是本补丁新增确定回归。未修改该测试或 Feishu 代码；保留原失败证据并复跑全仓。
- 在不改 Feishu 的情况下重跑全仓 `umask 022; go test ./...` 已成功（exit 0）；原偶发与安装基线反例记录保留，不隐去首轮失败。
