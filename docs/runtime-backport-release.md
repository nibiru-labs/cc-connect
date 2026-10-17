# 安装基线 runtime backport：构建与切换准备

此文是执行方案，不实施部署。当前安装来源为 `1ffc86d604eaf48fbcb5d2b000971d2222a1092d`，已观测二进制 SHA256 `afe08e95e251c844f3eb9e3d3de0fa60839dbb7633eeb67e5b19b44dc610b8f7`；切换时 root 仍须重新核对现物，不能把历史摘要当现场结果。main 虽包含此祖先，已删除 A2A/Nibiru 定制，不可替换安装来源。

## 源码与构建可复核

1. 仅检出已精确审查的 **backport head**，运行 `python3 tools/runtime_snapshot_source_guard.py`。它检查除原有 12 路径 allowlist 外的安装基线 tracked 文件原样保留，拒绝未知路径，并要求 clean tree。七个 runtime 文件逐字节来自 PR16 合并提交 `efad8d569ca3426b24fcdab6a3b7dbfe00f88bf6`；`core/engine.go` 必须匹配 PR20 `9540a1b` 基础加 PR16 relay lifecycle 增量的固定组合 blob，`core/engine_test.go` 仍匹配 PR20 blob `71de554a`。不要整文件复制 engine，也不从 main 复制整树。
2. 用 root 白名单核验现场二进制的 Go buildinfo：只取 Go toolchain、GOOS、GOARCH、CGO_ENABLED、build tags、vcs.revision、vcs.modified。不打印 ldflags、配置、环境或凭据。核现架构与原编译功能后选择相同目标及必要 tags；不得添加 `no_a2a` / `no_codex`。默认构建包含两插件。Makefile 的平台列表不列 a2a，不能把筛选变量当精确白名单。
3. 在隔离 checkout 构建到 **工作目录外的候选路径**。不运行 `nibiru/deploy/build.sh`：它会替换安装路径。以该项目既有 `no_web` 纯 Go 配方举例（root 须先核现场 tags；这不是默认删减功能的许可）：

   ```sh
   CC_REV=$(git rev-parse HEAD)
   CC_BUILT=$(git show -s --format=%cI HEAD)
   CC_VERSION=v1.5.0-836-runtime-backport
   go build -trimpath -tags no_web -ldflags "-s -w -X main.version=$CC_VERSION -X main.commit=$CC_REV -X main.buildTime=$CC_BUILT" -o /absolute/work/candidate/cc-connect ./cmd/cc-connect
   go version -m /absolute/work/candidate/cc-connect
   shasum -a 256 /absolute/work/candidate/cc-connect
   ```

   记录完整命令、Go toolchain、显式 GOOS/GOARCH/CGO_ENABLED、候选摘要及 head；buildinfo 必须为该 head、`vcs.modified=false`。`go list -deps -tags no_web ./cmd/cc-connect` 必须同时包含 `github.com/chenhg5/cc-connect/platform/a2a` 与 `github.com/chenhg5/cc-connect/agent/codex`。没有必要为了构建启动候选服务。
4. 若需默认含 web 构建，按 Makefile 先构建 web/dist，再去掉 no_web；记录前端依赖工具/锁文件与 dist 摘要。候选摘要是具体构建结果，不声称不同工具链/前端产物必然字节相同。交叉构建只能证明可生成目标文件，不能代替 gocc 原生执行验收。

## 不打断真实忙碌回合

- root 暂停向 Sakura **新派源码任务**，让已接任务完成；先收 Sakura 本人明确的任务完成、无在跑 turn/子任务声明和对应结果绑定。旧 `/sessions` 是连接列表，不能用其数量判断忙闲。若仍忙或状态未知，保持当前二进制，候选可继续离线验证。
- root 排定已获授权的切换窗口，协调用户直连与 A2A 入站，避免确认空闲后又进入新回合。未能稳定排空时不强杀、不中断。未部署快照前不存在可据以宣称实际 turn 已全部空闲的新接口。
- 切换前只核现有服务/二进制路径和受保护备份位置，备份当前实际二进制并记录摘要、属主、模式、启动参数引用；不复制或打印凭据。沿用现有启动方式及 config，禁止新增 listener/ACL/凭据、重复启动第二实例；不要把 Unix socket 上的只读快照实现当成修改服务权限的理由。
- root 确认切换后，通过原 socket GET /runtime 验证 schema/时间；新鲜 unsupported 必须 unknown/count=null。由 Sakura 开一个已授权真实任务，核 thread/turn/start 时间、running→waiting（若实际发生）→idle/closed；本人认证结果与绑定另核。常驻进程、连接或测试 turn 不作 30+ 实际开发任务证据。
- 若启动、A2A收发或 runtime 回归异常，在同样空闲/窗口约束下停止候选，恢复已备份的原二进制与原启动方式；核摘要与服务恢复，保留问题证据。旧安装文件/凭据/收件箱不删除、不轮换、不批量 ACK。实际部署、回滚及验收由 root 留痕，本文不标已执行。
