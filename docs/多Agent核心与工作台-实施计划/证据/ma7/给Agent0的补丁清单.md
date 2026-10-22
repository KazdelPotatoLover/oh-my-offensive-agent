# 给 Agent 0 的集成补丁

Agent 7 没有改这些文件。请按所有权接线，不要把本清单当成已经合并的实现。

## 需要挂载的合同路由

当前 `backend/cmd/server/main.go` 没有这些路由。验收测试服务器同样返回 404：

- `POST /api/runs/{runID}/commands`，要求 `Idempotency-Key`。A12 观察到 404。
- `POST /api/runs/{runID}/main/messages`。A08 观察到 404，正文是 `404 page not found`。
- `GET /api/runs/{runID}/workbench`。A09 和 A10 观察到 404。

Handler 实现归 Agent 5。挂载归 Agent 0，包括生产路由和测试路由。未实现端口保持失败关闭，不要接成静默兜底。

## Compose

`docker-compose.yml` 使用 `network_mode: host`。默认端口是 25432、28080、23000、29090，默认库是 `agent_workbench`。这些端口上已有开发服务。Agent 7 没有启动 Compose，也没有 prune。

发布 smoke 需要一组不碰撞的端口和独立数据库。在那之前，A16 的 Compose 项保持未验证。

## Playwright

共享 `frontend/playwright.config.ts` 仍由 Agent 0 和 Agent 6 编辑。Agent 7 的 spec 在 `frontend/tests/ma-acceptance/`，不在 `frontend/tests/e2e/`，共享 glob 不会收进去。

运行时必须设置：

- `MA_ACCEPTANCE_FRONTEND_URL`：隔离前端，不要指向 23000。
- `MA_ACCEPTANCE_BACKEND_URL`：隔离后端，合同预留端口 28187。
- `MA_ACCEPTANCE_DATABASE_URL`：必须包含 `agent_workbench_ma7`，不能是 `/agent_workbench`。

工作树没有 `frontend/node_modules`。这次没有执行 spec，也没有设置 `MA_ACCEPTANCE_BROWSER_RAN`。

## 不要做的接线

- 不要为了让 A01–A08 变绿而把发现模式改回串行兜底，或再加一套 Engine。
- 不要降低有效 worker 数来模拟 origin 限额。A06 观察到的模型峰值是 4，限额 2 还没有实现。
- 不要把本地模型替身的通过写成商业模型稳定。真实入口是 `go run ./internal/maacceptance/realsmoke`。缺少显式模型、目标和额度时它退出 2；当前提交在四者都设置时仍退出 3，不发起调用。

## 通知

- Agent 1：发现模式强制 N=1；创建路径丢弃请求里的 `worker_max_model_calls`；planner 调用期间不能领取；在途事实不能派出第二个方向。
- Agent 2：429 后没有有界 5xx 恢复；没有共享模型 in-flight 上限 2。
- Agent 3：HTTPS 种子被拒绝；身份隔离和 withdraw 未实现。A10 的峰值 1 先看 Agent 1。
- Agent 4：`run_command` 与 `execution_jobs` 缺失。
- Agent 5：planner steer/kill 工具和 Main/commands/workbench 路由。
- Agent 6：N=4 的浏览器流尚未跑。

Agent 7 没有新增迁移。026–030 继续保留为空号，不要建空文件。
