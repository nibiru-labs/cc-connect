# #1256 runtime 与消息修复的离线整合

Refs nibiru-labs/nibiru#1256、#1346、#1431、#969。

目标为 nibiru 分支，基线 b4dcc4632b06fd491f405440ab0d00cc9f613d89（含 #30/#31）。不切默认分支、不部署、不启动客户端、不新增 listener/权限/凭据。

## 契约和来源

- 移植 main efad8d569ca3426b24fcdab6a3b7dbfe00f88bf6 的 runtime 源码。在同一状态锁下采样协议 turn、开始/观察时间、waiting/closed/unknown。完成、失败、EOF 清除运行标识，迟事件不复活。
- 既有 Unix socket 的 GET-only /runtime 只输出运行元数据，无 prompt/project/session_key/账号/配置。不完整、unsupported、stale 的计数 null，不能当零；按 thread/turn 去重。保留 relay 起始、运行、后台 drain 的 inventory 生命周期。
- runtime 测试取已合 deploy/gocc-codex-runtime@667dc5165893274dcca946d5037ac5b580345fd8，其中 core 测试包含 #21 的每 worker 错误收集修复。
- 五个生产 Go 路径：agent/codex/appserver_session.go、agent/codex/runtime_snapshot.go、core/api.go、core/engine.go、core/runtime_snapshot.go；core/engine.go 只应用原 PR16 relay 增量，其余四个保持原 runtime 源。原 #30/#31 的消息熔断与 inflight 结算源/测试保留。
- 先仅加入 runtime 测试：缺 RuntimeSnapshot/handleRuntime 编译失败。移植后独立跑 runtime race 及既有消息回归；不以文本常量比对代替协议测试。

## 验收边界

本交付只形成同时含消息修复与 runtime 的源码候选。#1256 仍需绑定实际运行版本并取得新鲜 GET-only 快照；历史默认候选404不等于全机无任务。#1346/#1431/#969 现场验收仍 open。#1285 unsolicited idle 修复在另一安装分支，不混入本次最小 runtime 移植。

旧 deploy 的 source guard 仍要求 core 测试 byte 等于 PR16，而 #21 已合法改动，因此当前 guard 拒绝；本候选不复制该过期部署工具，不放宽其门禁，不声称通过其发布验收。
