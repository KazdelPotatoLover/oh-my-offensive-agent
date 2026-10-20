# Responses 实现与验收

实现日期：2026-10-20。实现目录：`oh-my-offensive-agent`。

## 已完成的链路

```text
创建 Run → 固定协议/模型/API 根地址快照 → Provider 工厂
                                              ↓
Engine / AgentRunner → SessionRunner → ResponsesProvider → /responses
                             ↓                 ↓
                       保存会话消息        解析原生 output Items
                             ↓                 ↓
                       验证并执行工具 ← 归一化 function_call
                             ↓
                  带 call_id 的 function_call_output
                             ↓
                  原生 Items + 工具结果 → 下一轮模型
```

Engine 负责 Planner/Worker 的调度，SessionRunner 负责单个会话的模型与工具循环；两者共用 Provider 接口。新增适配器处理 Responses 的网络协议，不另建一套执行引擎。

关键实现：

- `backend/internal/provider/config.go`：显式选择协议，默认保留 Chat Completions；创建 Run 时验证配置，执行时使用记录的快照。未知格式或错误配置不会切换到 Mock。
- `backend/internal/provider/responses.go`：非流式、`store:false`、完整客户端历史、函数工具定义与调用；显式请求 encrypted reasoning 并原样保存。暂不支持托管工具、流式或服务器管理会话。
- `backend/internal/agent/session_runner.go`：复用工具参数校验、权限检查、预算、取消和证据持久化。存在函数调用时优先进入工具循环；拒绝、截断或无最终答案不会完成会话。
- `backend/migrations/010_responses_state.up.sql`：增加 `agent_messages.provider_payload`、`model_calls.response_id`、`usage_known`。原始上下文按 Session 保存，不通过普通消息 API 暴露。
- `RunService.LoadSessionConversation`：从数据库恢复单个 Session 的历史，重建相同的后续请求。此阶段不自动恢复中断的 Run。
- 前端：显示当前 Run 的协议和模型，分别展示 HTTP Request ID 与 Response ID；缺失的用量显示为未知。

另外修复了原有模型名快照被局部变量遮蔽的问题；移除了 `CreateObservationWithAsset` 中重复且不可达的一条 return，使 `go vet` 可以通过。

## 本地配置

在项目根目录 `.env` 中配置，密钥只保存在本地：

```dotenv
LLM_API_FORMAT=responses
LLM_BASE_URL=https://your-gateway.example/v1
LLM_MODEL=服务商支持的模型ID
LLM_API_KEY=本地填写
```

Base URL 是 API 根地址，程序追加 `/responses`；不要填写完整的 `/responses` 路径。当前不接受包含用户凭据、查询参数或 fragment 的 Base URL。其他配置保留现有端口和数据库设置即可。

```bash
docker compose up -d --build backend frontend
```

若只是修改 `.env`，执行 `docker compose up -d backend` 重新创建后端即可。前端创建 Run 时选择「真实模型 API」。选择 Mock 仍然使用预设回复。

切回原来的协议：`LLM_API_FORMAT=chat_completions`。修改配置仅影响新 Run；既有 Run 保留自己的协议快照。

## 验收结果

已通过：

- 独立临时 PostgreSQL 数据库中的完整后端回归：90 项测试（含子测试），零失败，启用 `-race`。测试后删除临时数据库。未清空开发数据库。
- 补充的非法函数参数、未完成函数 Item、取消与超时等 Provider 用例，以及 `go vet ./...`。
- 前端 `npx tsc --noEmit`。
- Docker Compose 前后端生产构建与本地部署通过；迁移版本为 `10`，`dirty=false`。
- 部署后的 `/health`、前端 `/tasks` 和同源 API 代理均返回 HTTP 200；临时 Mock Run 完成 3 次模型调用，真实模型缺少配置时返回 HTTP 400。临时任务已删除。
- 本地 HTTP stub 验证 `/responses`、鉴权、扁平函数定义、工具 `call_id` 往返、原始 Items 顺序、phase 和加密状态保存。
- 两个并发 Session 共享 Provider，使用相同 call_id 仍互不混淆；从数据库读取历史后，新建 Provider 发出相同后续请求。
- HTTP 创建真实 Provider Run 的入口、配置快照、最终文本与持久化闭环。
- 截断、拒绝、重复 call_id 和工具预算超限时不会执行工具；401/404/429/500 不会切换协议或 Mock。
- 原有 Chat Completions、Mock、编排与资产/证据回归。

完整回归命令：

```bash
# TEST_DATABASE_URL 必须指向独立测试库；测试会清空其中的数据。
go test -race ./... -skip '^TestDiscovery_RealTargetRange_' -count=1 -json -timeout=180s
```

执行时跳过会主动访问靶场的 `TestDiscovery_RealTargetRange_` 测试。本轮不重复向靶场发送请求。

尚未验证：真实服务商的 Responses 工具循环及真实模型的靶场编排。本地 `.env`、Shell 和后端容器中均未配置模型地址、模型名或密钥，因此没有发送真实模型请求。

## 下一步

配置服务商凭据后，先用固定样本任务验证「模型调用工具 → 工具结果返回模型 → 最终答案」。确认 Model Calls、Tool Calls、用量和会话记录正常，再使用已有靶场做限制页面数与调用次数的小范围编排。第三方网关的实际兼容性必须以这两次验收为准。

协议依据：[官方迁移说明](https://developers.openai.com/api/docs/guides/migrate-to-responses)、[函数工具调用](https://developers.openai.com/api/docs/guides/function-calling)、[推理上下文](https://developers.openai.com/api/docs/guides/reasoning)。
