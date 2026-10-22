# #949 / #475 Codex 配额缓存的新鲜度与只读出口

范围是当前 nibiru 的 session UsageReporter，不将其误称 cmdUsage 的 agent 级 HTTP 路径。真实 app-server 配额 read 成功或 account/rateLimits/updated 被本地接收时记录 ObservedAt；这是本地收到上报的时刻，不证明 provider 全局采集时刻。缓存读取绝不刷新该时刻。

刷新失败必须返回 error，保留旧数据供诊断但同时在共享缓存记 Stale，不能让另一路 cache-only 读取继续声称新鲜；仅下一次成功 read/notification 重置。缺失、关闭、未来/超过120秒、无窗口或异常百分比都不可当有额度。

复用原 Unix API，仅新增 GET /quota，不新增 listener/认证/后台刷新。仅当现有运行库存恰有一个可枚举 session 且实现可选 cache-only 配额接口时，投影 source、as_of、rate_limits 中匿名窗口的 used_percent；多 session/unsupported/未知一律返回空窗口对象（不是null payload）。不暴露账号、项目、session/thread、plan、credits、prompt或错误正文。

输出直接兼容既有 tools/sakura/codex_dispatch_gate.py：带真实本地接收时间，保留20/5阈值，缺失/旧数据unknown拒绝；不触发派单或恢复旧worker角色。以合成协议事件/失败context、API及实际Go输出→Python gate验证，不调用真实模型/配额服务或读取OAuth。

## 本次验证与限制

- 真实 RED：刷新失败返回 cached,nil；最小负例先失败。
- 修复后对真实 notification/read 消费路径进行合成验证，包含失败共享失效、成功重置、读取不刷新时间、关闭/旧/未来/缺失/坏百分比、并发快照、HTTP方法与多来源拒绝。
- 已用真实 Go 序列化的 source/as_of/rate_limits 快照直接调用既有 Python gate；20%边界仍放行、19%/5%停派、4%停用，失败缓存unknown拒绝。没有运行worker_fill或派单。
- 三包完整race的首次runner曾在sandbox之后覆盖PATH，破坏gh97哨兵，已保留但撤回该轮完整沙箱证明资格（incident nibiru-labs/nibiru#1581）。纠正版只在既有sandbox PATH前追加codex哨兵，并先验证gh/codex均exit97；完整三包exit0，含子项1981 PASS/9既有SKIP。旧轮是否存在未观测外联仍unknown，不据新一轮绿洗掉历史。
- 当前交付不证明生产source已加载、真实配额或自动调度已恢复。ObservedAt只证明本地收到了此次报告；接口多来源拒绝而不猜账户归属。目标产品分支为nibiru，部署及现场验收另行进行。
