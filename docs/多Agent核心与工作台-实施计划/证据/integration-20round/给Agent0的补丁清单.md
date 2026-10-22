# 给 Agent 0 的补丁清单（20 轮复验）

日期：2026-10-22。分支 `codex/ma-integration`。测量基线 `63f6f92`。没有改 Engine、Handler、Provider 或 `协调状态.md`。

## 验收脚本修正（Agent 7 已在本分支改测试，不是产品补丁）

1. A07 跟进调用改为发送提示里的 `work_item_id`。合同 `Command` 要求这个字段。旧替身只发 `work_item_key`，会把参数错误记成 kill 失败。20 轮里 follow-up 次数是 0，所以这条修正还没有测到产品 kill。
2. A08 不再把人类 `POST /api/runs/{id}/commands` 的 `grant_budget` 200 当成 Main 越权。聊天加预算仍断言 grant 数为 0。第一次复验把 queued Run 交给 `PauseRun`，产品按状态拒绝，该 JSON 留在 `A08-queued-setup.json`。对着 running Run 的 20 轮复跑通过，证据是 `A08.json`。
3. A16 不再把已落地的 026/028/029/030 当成空保留号。027 必须继续不存在。发布门禁和旧库比对仍失败。
4. 证据默认目录从 `证据/ma7/` 改为 `证据/integration-20round/`，避免覆盖合入前基线。

## 仍要 owner 修的产品缺口

- Agent 1：Planner 调用期间 Dispatcher 不能领取剩余方向；worker 被挡住时 follow-up 不开始；A 未退出时多数轮次派不出 B；stop 后 running 子记录还在。
- Agent 2：共享模型 in-flight 上限没有压到 2。N=4 峰值为 4，stop 后 4 个请求和 4 条 running 子记录仍在。429 后没有 5xx 恢复。
- Agent 3：`service/run.go` 仍拒绝 HTTPS 种子。finish 复测仍要求结构化 `source_tool_call_id`。withdraw 路由不存在。
- Agent 4 / 0：验收用的 `setupTestEnv` 没有走 `composeCaptureAndJobs`。broker 未配置时 `run_command` 和 `http_request` 不注册。不要在测试里旁路 `RegisterAdapters` 做绿。生产启动路径需要能在隔离 broker 上挂上这两个工具。
- Agent 5：kill/steer 工具已在 planner 请求里，但 follow-up 没开始，目标方向保持 running。Main 对 queued Run 的暂停拒绝是符合 `PauseRun` 的。
- Agent 6：浏览器验收未跑。共享 Playwright 配置不要和本目录 spec 同时改。

## 发布门禁

不要启动根 `docker-compose.yml` 的默认端口。25432、28080、23000、29090 上已有开发服务。Compose 使用 host network。本轮没有 `config` 之外的启动，也没有 prune。

旧库 `agent_workbench` 没有迁移、复制或改写。
