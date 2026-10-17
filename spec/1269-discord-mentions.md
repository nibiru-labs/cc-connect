# Discord 不可信文本禁止触发提及通知（nibiru#1269）

父任务：nibiru-labs/nibiru#7；子项：https://github.com/nibiru-labs/nibiru/issues/1269。

## 问题与范围

父规格 `spec/comms/7-discord.md` 已要求每片不可信正文使用 `allowed_mentions: {"parse": []}`。当前原生 Discord 平台的普通发送、回复、interaction、按钮与预览路径未显式设置该字段，正文里的 `@everyone`、`<@id>` 与角色提及可能产生意外通知。

仅修改 Discord 平台输出 payload；保留正文、分片及 reply/thread 路由。不是新增账号权限收紧，不修改 allow_from、会话身份、A2A、真实 Discord 配置、PR15 unified_session 或 PR16 runtime。附件无正文路径不属于本项。

## 验收

1. 普通 Send、Reply、thread 和分片每片发送显式 `allowed_mentions.parse=[]`、`replied_user=false`，不声明可提及 user/role。
2. interaction 原始回复编辑、后续回复及失败 fallback 都使用相同策略；保持原 fallback 和错误行为。
3. 有按钮的普通/interaction 正文、点击按钮后的原消息更新和预览创建/编辑（纯文本及进度卡）同样显式携带该策略。
4. 原正文逐字保持；正常引用仍保留 `message_reference`，thread 仍投到原 thread，不靠删除正文禁止提及。
5. 先以假 HTTP 记录 payload 复现失败，再实现；平台回归、项目所需检查通过。

## 验证边界

测试仅使用本机假 HTTP 服务、合成内容与假 session，不接 Discord、A2A 或真实账号。测试通过不代表真实通知行为、生产上线或父 #7 AC1–AC11 已验收。后续部署由统筹独立安排。

回滚：还原本项 Discord 输出 payload 修改；无配置/凭据或存储迁移。

## 本次验证记录

- 合成 HTTP 回归 `TestOutgoingTextExplicitlySuppressesMentions`、`TestOutgoingSplitAndProgressPayloadsSuppressMentions` 在实现前因缺 `allowed_mentions` 失败；修复后通过。独立预审发现点击按钮更新出口也会重发原文，新增 `TestComponentUpdateExplicitlySuppressesMentions` 再次先失败后修复通过。
- `/workspace/work/go/bin/go test ./platform/discord -run 'TestOutgoing|TestComponentUpdateExplicitly' -count=1`：通过；覆盖原文字节、分片、引用与 thread 路由。
- `/workspace/work/go/bin/go vet ./platform/discord`：通过；`git diff --check`：通过。
- 先在 `web/` 执行 `npm install --no-package-lock --no-audit --no-fund`、`npm run build` 生成被忽略的真实 web/dist，再以进程内 `umask 022` 执行 `go test ./...`、`go build ./...`：均退出 0。执行时移除 GH_TOKEN/GITHUB_TOKEN；未使用真实 Discord token。
- 首次全仓运行因尚未生成 web/dist（embed 无输入）及默认 umask 077 导致既有 daemon fixture 的 0644 前置断言失败；补齐构建前置并调整仅该检查子进程的 umask 后完整通过，没有修改 daemon 或 web 源码。
- 独立子代理只读增量预审未发现剩余文本出口遗漏或明显回归；正式 PR 精确 head 审阅由统筹安排。
