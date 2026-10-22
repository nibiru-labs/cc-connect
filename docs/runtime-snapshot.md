# Unix runtime 快照（nibiru#1256）

现有本地 Unix API 提供只读 `GET /runtime`，沿用原 `api.sock` 与其权限。它与管理 HTTP API `/api/v1/status` 不同，不新增 listener。其他方法返回 405 / `Allow: GET`。

响应 `type=cc-connect.runtime.v1`，包含 `observed_at`、`complete`、`actual_running_turns` 和 `sessions`。会话字段为 `thread_id`、`turn_id`、`status`、`started_at`、`observed_at`、`alive`、`complete`。这些是协议运行元数据，不包含项目名、会话键、prompt、账号、配置或历史。

- `running`：已确认协议 turn，具有 ID 与开始时间。
- `waiting_input`：当前 turn 等待审批或用户回答，不算正在执行。
- `idle`：完整采样时没有 active turn；连接仍可存活。
- `closed`：连接已终止，运行 ID 与开始时间清除。
- `unknown` / `complete=false`：能力不支持或信息不完整，不能计为零。

运行数按 `(thread_id, turn_id)` 去重，不按连接、进程或子代理自报计数；任一源不完整则顶层计数为 `null`。空的已知枚举为完整零项。API 拒绝零观察时间、超过五秒的观察、超过一秒的未来观察以及不一致状态；消费者仍须对缓存响应检查新鲜度，不能长期复用一次结果。关闭、空闲和等待不会贡献运行数。

`RuntimeSnapshotProvider` 是可选能力，未实现的 agent 继续兼容并返回 unknown。快照接口不主动发请求或读取会话正文。源码或单测通过不代表生产二进制已部署，也不代表实际达到了并行目标。
