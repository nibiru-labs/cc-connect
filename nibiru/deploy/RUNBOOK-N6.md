# N6 切换 runbook：runner → cc-connect a2a 平台（演练清单）

本文件是 nibiru#836 N6 的操作清单，只描述步骤，不执行任何东西。
脚本见 `nibiru/deploy/`（`build.sh`、`switch-to-a2a.sh`、`rollback-to-runner.sh`、`lib.sh`、`config.a2a.example.toml`、`deploy_test.sh`）。
脚本的 `--dry-run` 只打印命令，不调用 launchctl 的写操作。

**铁律**：同一个 A2A 收件箱只允许一个消费者。`com.nibiru.sakura-runner` 与 cc-connect 的 a2a 平台不能同时加载。
任何步骤让两者同时存在，或让两者都不在，都按失败处理（见各步“失败判据”）。

## 0. 角色与范围

| 项 | 内容 |
|---|---|
| 执行人 | 运维操作者（本机 gocc 的用户）。本 runbook 不由 worker 代跑。 |
| 生产主机 | gocc（正式 `com.cc-connect.service`、`com.nibiru.sakura-runner`）。 |
| 测试机 | **待定（卡点）**：脚本硬编码 `gui/<uid>` 与 `com.cc-connect.service` / `com.nibiru.sakura-runner` 标签，演练必须在独立的 macOS 用户或独立主机上进行，不能在生产 launchd 域里跑真实 bootout。没有测试机时，演练只到 dry-run 与 `deploy_test.sh`。 |
| 影响 | 切换期间 cc-connect 管理的钉钉会话会随 `bootout` 中断，需要用户知悉。A2A 收件箱在两步之间没有消费者，窗口应控制在分钟级。 |

## 1. 演练（不影响线上）

演练与正式切换分开做。演练通过后才进入第 2 节。

### 1.1 脚本自测（任何机器都能跑）

```sh
bash nibiru/deploy/deploy_test.sh
```

- 预期：25 行 `ok`，无 `FAIL`，退出码 0（已于 2026-10-15 在 `9f5c5223` 上验证：25 ok / 0 FAIL / rc 0）。
- 失败判据：任何 `FAIL` 或非零退出码。停止，不进入后续步骤。
- 该测试在临时 `HOME` 下运行，不碰 launchd 与真实 `~/.cc-connect`。

### 1.2 构建 dry-run（生产主机，只打印）

```sh
nibiru/deploy/build.sh --dry-run --tag <tag>
```

- 预期输出包含：`-tags no_web`、`main.version=<tag>-nibiru`、输出路径 `~/.local/nibiru/bin/cc-connect`、`cc-connect.bak-<ts>` 备份行。
- 失败判据：缺少上述任一项，或输出里有非预期的写操作。

### 1.3 切换 dry-run（生产主机，只打印）

```sh
nibiru/deploy/switch-to-a2a.sh --dry-run
```

预期输出按顺序包含（已用临时 `HOME` 验证过）：

1. `cp -p .../config.toml .../config.toml.bak-a2a-<ts>`
2. `cp -p .../com.nibiru.sakura-runner.plist .../com.nibiru.sakura-runner.plist.bak-a2a-<ts>`
3. `launchctl bootout gui/<uid>/com.nibiru.sakura-runner`，然后 `+ confirm com.nibiru.sakura-runner unloaded`
4. `launchctl bootout gui/<uid>/com.cc-connect.service`，然后 `+ confirm ... unloaded`
5. `launchctl bootstrap gui/<uid> .../com.cc-connect.service.plist`
6. 末行 `switched. verify: ...`，退出码 0

失败判据：runner 的 bootout 不在 cc-connect 的 bootstrap 之前，或缺少任一行。停止。

**注意**：dry-run 不检查前置条件（那些检查只在非 dry-run 时执行）。dry-run 通过不代表第 2 节的前置检查通过，两者都要做。

### 1.4 回滚 dry-run（生产主机，只打印）

```sh
nibiru/deploy/rollback-to-runner.sh --dry-run
```

预期输出按顺序包含：`bootout com.cc-connect.service` → `confirm ... unloaded` → `cp` 当前配置到 `config.toml.bak-pre-rollback-<ts>` → `cp` baseline 到 `config.toml` → `bootstrap` cc-connect plist → `bootstrap` runner plist → `rolled back.`。

失败判据：缺任一步，或 bootstrap 顺序颠倒（runner 先于 cc-connect）。

### 1.5 测试机真演练（有测试机时执行）

在测试机独立的 macOS 用户下，按第 2 节的全部步骤走一遍，包括：

- 用假 A2A 服务（`nibiru/e2e/`，L8 产物）投递两条同会话消息，确认第二条带上第一条上下文；
- 切换后执行一次回滚，再切换一次，确认每次都只有一个消费者；
- 记录每一步的实际输出与预期的差异，写进演练记录（第 5 节）。

**卡点**：测试机未定；`nibiru/e2e/` 与 `config.a2a.example.toml` 的选项名依赖 `platform/a2a/a2a.go`（nibiru-labs/cc-connect#7），当前 `nibiru` 基线里只有 `ack.go`、`pong.go`，尚未包含 `a2a.go`。在 #7 合并前，选项名未经验证，1.5 不能完整通过。

## 2. 正式切换

### 2.0 前置检查（只读，任何一项不满足都不切换）

以下命令都是只读的。**不要**为了检查而运行 `a2a read --pending`：它可能改变收件箱的投递状态，等于再开一个消费者。

| # | 检查 | 命令 | 预期 | 不满足时 |
|---|---|---|---|---|
| P1 | 二进制存在且版本正确 | `~/.local/nibiru/bin/cc-connect --version` | 输出 `<tag>-nibiru` | 先跑 `build.sh`，不要切换 |
| P2 | runner 当前在跑 | `launchctl print gui/$(id -u)/com.nibiru.sakura-runner \| grep -E 'state\|pid'` | `state = running`，有 pid | 若不在跑，先确认原因，不要切换 |
| P3 | cc-connect 当前在跑 | `launchctl print gui/$(id -u)/com.cc-connect.service \| grep -E 'state\|pid'` | `state = running`，有 pid | 切换脚本会 bootout 再 bootstrap；先确认 cc-connect 本身健康 |
| P4 | baseline 存在且不含 a2a | `grep -c 'type *= *"a2a"' ~/.cc-connect/config.toml.runner-baseline` | `0`（且文件存在） | 按 2.1 生成 |
| P5 | 当前配置已合入 a2a 项目 | `grep -c 'type *= *"a2a"' ~/.cc-connect/config.toml` | `≥1` | 手工合入 `config.a2a.example.toml`（见 2.1），脚本不改配置 |
| P6 | runner 没有未完成回合 | `tail -n 20 ~/.config/nibiru-a2a/runner.log` | 最后一轮已结束，没有进行中的回合 | 等待本轮结束再切；不要中途切 |
| P7 | plist 指向新二进制 | `grep -A1 ProgramArguments ~/Library/LaunchAgents/com.cc-connect.service.plist` | 路径是 `~/.local/nibiru/bin/cc-connect` | 不一致则停止，先修正 plist（由人工改，脚本不改） |
| P8 | 模型与工作目录已确认 | 打开合入后的 a2a 项目段 | `model = "sonnet"`，`work_dir` 为预期的仓库目录，`mode` 为预期值 | 样例里是 `/Users/surlymo/workspace/nibiru` 与 `bypassPermissions`；由操作者逐项确认 |
| P9 | 留痕文件 | `mkdir -p ~/.config/nibiru-a2a && date` | 目录存在 | 见第 4 节留痕要求 |

### 2.1 准备（一次性，切换前做）

```sh
cp -p ~/.cc-connect/config.toml ~/.cc-connect/config.toml.runner-baseline
grep -c 'type *= *"a2a"' ~/.cc-connect/config.toml.runner-baseline   # 必须输出 0
```

- 预期：baseline 是合入 a2a 之前的配置，不含 a2a 平台。
- 失败判据：输出非 0。说明 baseline 来源不对，停止。baseline 必须在合入 a2a 项目之前保存。
- 然后手工把 `config.a2a.example.toml` 中的 `[[projects]]` 段合入 `~/.cc-connect/config.toml`。脚本不改配置，合入是人工动作。
- 合入后执行 P5。

### 2.2 切换 dry-run

重复 1.3。这一步与 2.0 的前置检查一起完成，两者都通过才往下走。

### 2.3 正式切换

```sh
nibiru/deploy/switch-to-a2a.sh
```

脚本的执行顺序（这是本 runbook 的核心约束）：

1. 备份 `config.toml` 与 runner plist（`.bak-a2a-<ts>`）；
2. bootout `com.nibiru.sakura-runner`，并确认已卸载；未卸载则中止，cc-connect 不动；
3. bootout `com.cc-connect.service`，并确认已卸载；未卸载则中止，runner 保持停止；
4. bootstrap `com.cc-connect.service`。

预期输出：末行 `switched. verify: tail -f ~/.cc-connect/logs/cc-connect.log ; rollback: rollback-to-runner.sh`，退出码 0。

失败判据与处理：

| 现象 | 当前状态 | 处理 |
|---|---|---|
| `refusing: ... still loaded` 或 `aborted: old consumer still running` | runner 仍在跑，cc-connect 未动 | 正常，有单一消费者。查明 runner 为何卸不掉，再重试。不要手动 kill。 |
| `aborted: cc-connect still loaded; runner stays stopped` | **没有消费者**（runner 已停，cc-connect 未重载） | 立即执行 2.4 回滚，把 runner 拉回来。 |
| bootstrap cc-connect 失败，或末行不是 `switched.` | **没有消费者**（runner 已停） | 立即执行 2.4 回滚。 |
| 脚本成功，但 cc-connect 启动后崩溃循环 | 无 runner，cc-connect 异常 | 执行 2.4 回滚。 |

**没有消费者的窗口**：runner 已停而 cc-connect 未就绪时，A2A 消息会堆在收件箱里，不会丢失（未 ack 的消息会再次出现）。窗口内必须尽快完成回滚或修复，不要长时间停留。

### 2.4 切换后验证

按顺序做，每一步都有失败判据。

| # | 检查 | 命令 / 动作 | 预期 | 失败判据 |
|---|---|---|---|---|
| V1 | cc-connect 在跑 | `launchctl print gui/$(id -u)/com.cc-connect.service \| grep -E 'state\|pid'` | `running` | 非 running，或 pid 持续变化 |
| V2 | runner 已卸载 | `launchctl print gui/$(id -u)/com.nibiru.sakura-runner` | 非零退出（服务未加载） | 仍能打印出 state，说明两个消费者可能同时存在 → 立即 2.4 回滚 |
| V3 | a2a 平台启动 | `tail -n 200 ~/.cc-connect/logs/cc-connect.log` | 有 a2a 平台启动的日志，无 `panic` / `fatal` | 无启动日志，或出现 panic |
| V4 | 无重复回复 | 观察日志 10 分钟 | 没有同一 `message_id` 被回复两次 | 出现重复回复（两个消费者的迹象）→ 立即 2.4 回滚 |
| V5 | 功能冒烟 | 由名册内成员（`allow_from` 默认 `grok,dot,claude-cloud,hermes`）发一条测试消息 | 收到回复；该消息不再出现在 `a2a read --pending` 中 | 无回复，且日志出现 `a2a: turn failed, message will be redelivered` 持续 3 次以上 → 2.4 回滚 |
| V6 | 非名册被丢弃 | 由名册外的发件人发一条消息（可选） | 日志记录后丢弃，不进模型 | 进入了模型（有回复）→ 立即 2.4 回滚 |
| V7 | 回复失败不 ack | 人为制造一次回复失败（可选，测试机做更安全） | 日志出现 `a2a: ack failed, will retry` 或 `turn failed`，消息再次出现 | 消息被 ack 但未回复 |
| V8 | 钉钉通道正常 | 在钉钉项目发一条普通消息 | 正常回复 | 钉钉通道异常，且与 a2a 无关 → 记录，不一定回滚（由操作者判断） |

验证通过后，在第 4 节的留痕里记录结果。

### 2.5 回滚触发条件

满足任一即回滚，不需要再征求意见：

- V2 发现 runner 仍然加载，或 V4 出现重复回复（两个消费者）；
- V3 无启动日志，或 cc-connect 崩溃循环（launchd 在短时间内反复重启，例如 10 分钟内 3 次以上）；
- V5 或 V6 失败；
- 2.3 中任何一种“没有消费者”的状态。

以下情况由操作者判断，不自动触发回滚：V8 钉钉异常但原因不明（先查 `~/.cc-connect/logs/cc-connect.log` 与 dingtalk 相关日志，确认与 a2a 无关再决定）。

### 2.6 回滚

回滚脚本的作用：确认 cc-connect 已卸载 → 把 baseline 恢复为 `config.toml` → bootstrap cc-connect → bootstrap runner。

```sh
nibiru/deploy/rollback-to-runner.sh --dry-run    # 先看
nibiru/deploy/rollback-to-runner.sh               # 再执行
```

预期输出：`rolled back. verify: tail -f ~/.config/nibiru-a2a/runner.log`，退出码 0。

回滚的前置条件与失败判据：

| 现象 | 含义 | 处理 |
|---|---|---|
| `missing ~/.cc-connect/config.toml.runner-baseline; refusing` | 没有 baseline，无法安全回滚 | 停止。不要手工删配置或直接 bootstrap。找回 baseline 后再回滚。 |
| `... contains an a2a platform; refusing` | baseline 已被污染 | 停止。baseline 必须是切换前的配置。 |
| `runner is already loaded; a second consumer would start` | runner 已在跑 | 不要强行卸载后再来。先确认哪个是消费者，并记录。 |
| `refusing: ... still loaded after bootout` | cc-connect 卸载失败 | 脚本已中止，runner 未启动（安全）。查明原因，再重试。 |
| `restored config still has a2a; aborting before start` | 恢复后的配置仍含 a2a | 脚本已中止，未启动任何服务。人工检查 baseline 内容。 |

回滚后的验证：

- R1：`grep -c 'type *= *"a2a"' ~/.cc-connect/config.toml` 输出 `0`；
- R2：`launchctl print gui/$(id -u)/com.nibiru.sakura-runner | grep -E 'state|pid'` 为 `running`；
- R3：`tail -f ~/.config/nibiru-a2a/runner.log` 出现新的轮询记录；
- R4：从名册成员发一条测试消息，收到 runner 的回复。

回滚脚本会把切换前的 a2a 配置保存为 `config.toml.bak-pre-rollback-<ts>`，不删除任何文件。

## 3. 备份与回滚留痕

| 文件 | 何时产生 | 用途 |
|---|---|---|
| `~/.cc-connect/config.toml.runner-baseline` | 2.1，切换前手工 | 回滚的唯一依据，**不要改写** |
| `~/.cc-connect/config.toml.bak-a2a-<ts>` | `switch-to-a2a.sh` | 切换时的 a2a 配置，仅留痕 |
| `~/Library/LaunchAgents/com.nibiru.sakura-runner.plist.bak-a2a-<ts>` | `switch-to-a2a.sh` | runner plist 的备份（plist 本身不改） |
| `~/.cc-connect/config.toml.bak-pre-rollback-<ts>` | `rollback-to-runner.sh` | 回滚时的 a2a 配置，仅留痕 |
| `~/.local/nibiru/bin/cc-connect.bak-<ts>` | `build.sh` | 上一版二进制，回退二进制用 |

- 备份文件不被脚本清理。切换或回滚后不要删除，直到下一次成功切换稳定运行一周以上（建议值，操作者确认）。
- 注意：`*.bak-a2a-<ts>` 的 plist 备份位于 `LaunchAgents` 目录，launchd 只加载 `.plist` 结尾的文件，`.bak-a2a-<ts>` 结尾的不会被加载。
- README 中“plist 只读不改”指的是 plist 本身不改写；切换时会产生一份备份副本。

## 4. 留痕要求

每次演练与正式切换都要留下记录，记录放在 `~/.config/nibiru-a2a/` 下（生产主机）。

- 记录文件：`~/.config/nibiru-a2a/n6-switch-<YYYYMMDD>.log`，每一步的命令与实际输出用 `tee -a` 追加。
- 记录内容至少包括：时间（本地时区与 UTC）、执行人、构建 commit（`build.sh` 输出的 `commit:`）、`cc-connect --version`、每一步前置检查的实际输出、切换与验证的实际输出、回滚（如有）的实际输出、最终结论。
- 不要把凭据（`dingtalk.env`、token、API key）写进记录。记录只保留命令与状态输出，遇到含凭据的行先脱敏。
- 记录完成后，把结论同步到 nibiru-labs/nibiru#836 的评论。

## 5. 演练记录模板

演练（1.5）与正式切换各填一份，放在 `~/.config/nibiru-a2a/` 下：

```
场景：dry-run | 测试机 | 正式
时间：
执行人：
构建 commit：
cc-connect --version：
步骤编号 | 命令 | 预期 | 实际 | 通过/失败
1.1 deploy_test.sh | 25 ok / 0 FAIL | |
...
偏差说明：
回滚是否触发：是/否，触发原因：
结论：
```

## 6. 不在本 runbook 范围内

- 不改脚本，不改配置样例。脚本与样例的修改属于 L7（deploy 路）。
- 不包含 pong 与 ack 的实现细节，见 `platform/a2a/`（L2）。
- 不包含 M3 轮换与 status 注入（L4、L6）。
- 不包含 launchd plist 的生成；plist 由人工维护。

## 7. 已知问题与待办

- 切换脚本在 runner 停止后、cc-connect 未就绪时，没有自动回滚。目前靠 2.3 的人工判断与 2.6 回滚。待办：脚本失败时自动回滚（属于 L7，本路不改）。
- `config.a2a.example.toml` 的选项名（`a2a_bin`、`poll_interval`、`max_chunk`、`allow_from`）依赖 `platform/a2a/a2a.go`（cc-connect#7）。#7 合并前无法从本基线验证。
- 测试机未定，1.5 的完整演练无法执行。
- V7 与 V5 的回复判据依赖 `a2a: ...` 日志字符串，字符串来自 `platform/a2a/ack.go` 与 `pong.go`。L1 的 `a2a.go` 合并后需要复核日志是否一致。
- 文中的“连续 3 次”“10 分钟内 3 次”等阈值是建议值，由操作者确认后再使用。
