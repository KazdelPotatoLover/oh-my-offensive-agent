# M4 工作台证据

日期：2026-10-22。分支 `codex/ma-workbench`。这不是 M4 完成声明。真实后端还没有挂载 workbench.v1，下面的浏览器证据来自显式合同替身，不是真实 N=4 执行。

## 已运行

在 `frontend/`：

```text
node --experimental-strip-types --import ./tests/workbench/register.mjs --test --test-reporter=spec src/lib/workbench/workbench.test.ts
```

结果：10 pass，0 fail。第一次运行在模块落地前失败（`ERR_MODULE_NOT_FOUND`）。补上模块后，Node strip-types 把 `export type` 擦掉，值导入 `CommandBody` 失败；改为 `import type` 后通过。没有单独的红灯提交。

```text
npm run build
```

结果：Next.js 14.2.15 生产构建通过，`/workbench` 及 Task/Run/WorkItem 路由已生成。

```text
npm run test:workbench
```

结果：13 pass，0 fail。Playwright 启动 `127.0.0.1:23196` 合同替身和 `127.0.0.1:23186` Next。没有启动 Go，没有连接 `agent_workbench`，没有调用付费模型。截图底部红条写明“合同替身证据，不是真实后端 N=4”。

覆盖：4 张方向卡片与 requested/effective/active/queued 一致；6 条 model_calls 不成为卡片；源站等待不改写 effective；命令使用快照 Epoch 2，而不是记录 current_epoch 1；选中方向能看到 Attempt、`http_request`、产物正文；Kill 只改目标方向；暂停后停止仍可用，新 Epoch 另计；聊天 pending、HTTP 202、action 的 command_id、同钥匙不同正文 409；刷新保持选中；断线保留上次快照；旧记录 501 且统一命令禁用；终态 stopped 与 Epoch 2 同快照；复测 fixed 在 snapshot_at 变化后出现；`?task=` 进入规范路径，只有 `?run=` 时不丢 Run；新任务 N 默认 3，https 不改写成 http；旧运行页链接进入工作台；390px 宽度可切换导航/执行/检查。

## 真实后端探针

只读请求 `127.0.0.1:28080`，没有重启服务，没有写开发库。

- `GET /health` → 200
- `GET /api/runs/{id}/workbench` → 404，`text/plain`，正文 `404 page not found`
- `POST /api/runs/{id}/commands` → 404，同样的纯文本

这不是 Envelope。前端不会把这次 404 当成成功，也不会改走旧 pause/steer。

## 未验证

- 真实后端 N=4 的卡片与计数
- 真实 Main 对话、Kill、暂停后 Stop、Epoch 和复测
- 真实断线恢复
- 现有 `npm run test:e2e`（需要独立浏览器库和 Go 服务，本轮没有启动）

## 有意差异

- 生产数据只走 `/api`。合同替身只由 Playwright 启动，应用内没有静默 mock。
- 轮询 1.5 秒，没有 SSE。
- 旧运行页保留。首页仍是 `/tasks`。旧启动表单 N 默认 1，避免改写现有 e2e 的有效并发；新工作台创建默认 3。
- 没有第二套单/双 Worker 执行逻辑。
- 没有 workbench.v1 快照时，统一命令禁用。
- `accepted` 和 `delivered` 分开显示。
- 模型调用条数来自既有 `model_calls` 数组长度。workbench.v1 没有这个计数字段。失败时显示错误，不显示 0。
- 检查审核只在发现页。探索资料默认折叠。

## 给 Agent 0 的集成补丁

前端不改后端。需要别人落地的接线：

1. 在 `backend/cmd/server/main.go` 挂载并返回 Envelope，不要继续用 Go 的纯文本 404：
   - `GET /api/runs/{runID}/workbench`
   - `POST /api/runs/{runID}/commands`
   - `POST /api/runs/{runID}/main/messages`
   - `GET /api/runs/{runID}/main/messages`
2. `backend/internal/handler/run.go` 约 203 行仍把发现模式 `MaxParallelWorkers` 改成 1。前端按用户填写的 N 发送，并提示 effective 以快照为准。Agent 1 需要去掉这层改写。
3. `BudgetConfig` 没有 `planner_reserved_model_calls` / `main_reserved_model_calls`。`service/run.go` 的请求覆盖也没有复制 `worker_max_model_calls`、`worker_max_tool_calls`、`worker_timeout_seconds`。这些字段会被丢掉。工作台会列出差异，不会显示成已生效。
4. `service/run.go` 不把 `config.agents` 写入 `execution_input.agents`。需要 Agent 0/2 提升。缺字段时合同允许沿用旧 `agent_config`，但界面不能假装角色别名已经生效。
5. 主对话若需要表，迁移 030 属于 Agent 5。前端没有迁移。
6. 不要新增第二套 Engine，也不要用串行兜底掩盖并发。

## 迁移

前端无迁移。旧任务通过 `/tasks/{taskId}/runs/{runId}` 的“在统一工作台打开”进入。没有 workbench.v1 的旧记录仍显示历史工作项，统一命令保持禁用。
