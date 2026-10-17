# Codex active-turn 只读快照（nibiru#1256，父 #969）

## 目标
现有 Unix socket 新增 GET `/runtime`，不新增 listener 或权限。可选 `RuntimeSnapshotProvider` 不改变 AgentSession 兼容性。快照只输出opaque thread ID、turn ID、状态、UTC started_at/observed_at、alive、complete；不输出 project、session_key、prompt、账号、配置或历史。thread ID 为协议生成的非内容标识。

## 状态与验收
- Codex 同一 stateMu 下维护 turn ID、起始时间与完成代次。确认 start 响应或 turn/started 才运行；重复同 ID 不重置起始时间。
- complete/failed/idle 清除当前 turn；早于 start 响应的完成不得被迟响应复活。Close、读循环终止、transport abort 后状态 closed，不计运行。
- pending approval 或用户输入显示 waiting_input；空闲为 idle，unsupported 为 unknown/complete=false；未获协议 thread ID 不声明 complete。
- GET 以外 405。顶层 complete 仅在所有现有会话均支持完整快照时为 true；不支持或部分源 actual_running_turns=null，不能当零。重复同 thread+turn 只计一次。
- 并发读取与通知 race 测试；状态驱动测试覆盖运行、重复、完成、失败、等待、关闭、早完成迟响应及 API 无内容字段。

## 范围
只交付源码和测试；不部署二进制，不改现有 /sessions，不读取真实配置或消息。下游需按 observed_at 新鲜度判断，本响应为请求时即时采样，不包含平台额度或子代理推断。

## 验证记录

- RED：先加入 RuntimeSnapshot/API 状态测试，`go test ./core ./agent/codex -run Runtime -count=1` 因缺少接口/方法编译失败。
- GREEN：实现后同组目标测试与 `go test -race ./core ./agent/codex -run Runtime -count=1` 通过，覆盖 alias 只采样一次、二元组去重及闭合/旧通知/审批/EOF。
- `go test ./core -run TestCUJ -count=1`、`go vet ./core ./agent/codex`、`go build ./...` 通过。全仓测试需要先按 Makefile 流程构建 web/dist；测试 shell 使用 umask 022，以满足既有 systemd legacy 0644 前置条件。本次不提交生成产物或修改 daemon 测试。
