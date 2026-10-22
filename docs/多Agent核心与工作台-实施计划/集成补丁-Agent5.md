# Agent 5 集成补丁清单

给 Agent 0。本文件只描述接线，不代替实现。这些补丁尚未应用到 `cmd/server/main.go`、`handler/run.go`、`orchestration/engine.go`、`agent/session_runner.go`、`worker/manager.go` 或 `service/`。

依赖提交：

| 提交 | 内容 |
| --- | --- |
| `6252083f8385a5722fe689553a53f03da957ada9` | 协调基线 |
| `e7fe7a335a5075bc763fa672212d5fc03b469b62` | `ExecutionController`、监督工具工厂、replan 闭合、迁移 030 |
| `802e5d07b9266ec079418c6e6c125906359c1c8a` | Main Agent、workbench 快照、`handler/control_api.go`、`ma_control_*` 测试 |

行号来自 `codex/ma-control` 的 `802e5d0`，该提交仍含基线 Engine 与 SessionRunner。合并 `codex/ma-runtime` 后按符号重读，不要把这些行号直接套到运行时分支。

## 已交付、可直接调用的类型

- `controlplane.NewController(runs, live, replan, recover)` 实现 `contract.ExecutionController`。
- `controlplane.ManagerLive` 适配现有 `worker.Manager`。`Kill` 调用 `Manager.KillWorkItem`。它不调用 `Manager.SteerWorkItem`。
- `controlplane.NewMemoryReplan()` 是 M1 合入前的进程内 replan 端口。`NoteSteer` 不写第二行 steer。
- `controlplane.CloseUnexecutedBatch` 与 `ProtocolPairingOK` 负责协议闭合。
- `controlplane.NewSupervision(runs, controller)` 实现 `contract.ToolsetFactory`。`Registry(scope, principal, keyFn, mutations)` 返回可执行工具。
- `controlplane.NewWorkbench(runs, modelSource)` 实现 `contract.WorkbenchQuery`。`modelSource == nil` 时模型容量为零，不编造限额。
- `controlplane.NewMainAgent(runs, controller, workbench, provider, resolver)`。`resolver == nil` 时回合快照为 `{"fallback":"historical"}`。
- `handler.NewControlAPI` / `handler.MountControlAPI`。路径相对调用方，必须挂在已有 `/api` 组内。

公共路由：

- `POST /api/runs/{runID}/commands`，200，`contract.CommandResult`
- `GET /api/runs/{runID}/workbench`，200，`workbench.v1`
- `POST /api/runs/{runID}/main/messages`，202，`contract.MainMessageAccepted`
- `GET /api/runs/{runID}/main/messages`

POST 需要 `Idempotency-Key`。人类操作者来自 `X-Actor-Id`，缺省 `operator`。请求体不能携带 principal。`expected_epoch` 为 0 时拒绝，不改写成 1。

## 1. 启动工厂与路由

文件：`backend/cmd/server/main.go`。

1. 第 83 行 `AllowedHeaders` 增加 `X-Actor-Id`。`Idempotency-Key` 已在列表中。
2. 在第 89 行 `r.Route("/api", ...)` 内部、现有 run 路由之后调用：

```go
replan := controlplane.NewMemoryReplan()
controller := controlplane.NewController(runService, &controlplane.ManagerLive{Manager: manager}, replan, recoverRun)
workbench := controlplane.NewWorkbench(runService, modelCapacity) // 未接 Agent 2 限流器时传 nil
mainAgent := controlplane.NewMainAgent(runService, controller, workbench, provider, runtimeResolver)
handler.MountControlAPI(r, handler.NewControlAPI(controller, workbench, mainAgent))
```

3. 把同一个 `controller`、`controlplane.NewSupervision(runService, controller)`、`workbench` 填进 `contract.ExecutionPorts`。其它端口仍由对应 owner 提供。缺端口时 `ProductionReady` 继续失败，不要用成功替身填上。
4. `MountControlAPI` 不要挂到根 mux。挂到根上会变成 `/api/api/runs/...`。

`recoverRun` 复用 `handler/run.go` 第 1313–1382 行已有顺序，并返回 `(epoch, executorStarted, err)`：

1. `manager.QuiesceEpoch`
2. `runService.StartNewEpoch(ctx, runID, reason, summary, service.EpochStartOptions{ExpectedEpoch: expected, IdempotencyKey: key})`
3. `manager.RevokeEpoch(runID, epoch-1)`
4. `runHandler.startRunExecutor(run)`，函数在 `handler/run.go:137`

`executorStarted` 只在这一个 executor 启动成功时为 true。同一 `runID`，不新建 Run，不新建 Engine。Controller 在调用 `RecoveryFunc` 前已释放自己的 advisory lock；`StartNewEpoch` 使用自己的行锁。不要在持有 `0x4D330001` 或 `0x4D330002` 锁期间等待模型或进程退出。

M1 合入后，`replan` 换成 Engine 上那一个 `orchestration.ToolBatchSupervisor`。Controller、Engine、Manager 共用这一实例。`Manager.NoteSteer`（`ma-runtime` `worker/manager.go` 的 `NoteSteer`）只转发给它。不要再调用基线 `Manager.SteerWorkItem` 或 `Engine.SteerWorkItem`：当前基线 `Engine.SteerWorkItem`（`engine.go:132`）会再写一行 `work_control_messages` 然后 `Nudge`，且不武装 replan 端口。

## 2. Planner 工具注册

文件：`backend/internal/orchestration/engine.go`。

两处注册必须调用同一个 `Supervision`，工具名不能分叉：

- 初始轮 `plannerRegistry`，约第 375–397 行
- 后续轮 `evalRegistry`，约第 603–625 行

保留 `submit_plan`、目标、证明、待办、资产、产物读取和 `search_all_worker_traces`。去掉这两处的 `tools.NewContinueWorkItemTool` 与 `tools.NewRetryWorkItemTool`。它们的名字是 `continue_work_item` / `retry_work_item`，不走 `ExecutionController`。

用下面的 scope 调用 `Supervision.Registry`，`mutations` 为 true，然后只把控制工具注册进两个 registry。Planner scope 不能携带 `work_item_id` 或 `attempt_id`，目标写在命令上。

```go
scope := contract.ExecutionScope{
    RunID: e.runID, EpochNumber: e.Epoch(), Role: contract.RolePlanner, SessionID: &sess.ID,
}
principal := contract.Principal{
    Kind: contract.PrincipalPlanner, ActorID: "planner:" + sess.ID.String(), RunID: e.runID,
}
reg, err := supervision.Registry(scope, principal, nil, true)
```

注册后的控制名必须是 `add_intent`、`steer_work`、`kill_work`、`continue_work`、`retry_work`，外加已有读取工具。不要注册 `grant_budget`、`shell`、`start_worker`、`http_request`、`run_command`。两套名字都要能通过 `contract.RequirePlannerSupervision`。

`planSubmitted`（约第 371 行）和 `followUpPlanSubmitted`（约第 589 行）仍要在 `continue_work`、`retry_work`、`add_intent` 成功后置位。第 447 行把未提交计划视为本轮失败，第 705 行用后续标志决定是否调度。只 `Nudge` 不会改这两个局部变量。可以在成功的工具结果外包一层回调，不要恢复旧工具的直写路径。

后续轮提示词第 639、645 行仍写着 `continue_work_item`。改成合同名 `continue_work`，避免模型调用已删除的工具。

## 3. SessionRunner 在下一工具动作前闭合旧批次

文件：`backend/internal/agent/session_runner.go`。工具循环从第 555 行开始，预算检查结束于第 553 行，`beforeToolHook` 在第 572 行。`beforeToolHook` 只能中止整个会话，不能闭合单个调用，所以不要把 replan 放进这个 hook。

在每一次工具执行之前、`beforeToolHook` 之前调用 `DecideBeforeTool(workItemID, batch, executed)`。`executed` 是本批次已经执行的调用数。决策为 `CloseUnexecuted` 时：

1. 不再调用本调用及之后调用的 `tool.Execute`。
2. 用 `controlplane.CloseUnexecutedBatch(assistant, executed, decision)` 生成消息。顺序是未执行调用的 tool result，然后一条用户消息 `[Operator Steer Guidance]\n` + instruction。
3. 保留 assistant 的 `ToolCalls` 和 `NativePayload`。不要在 assistant tool_calls 与 tool result 之间插入用户消息。
4. 用现有 `InsertAgentMessageWithSession` 持久化这些消息，并给闭合调用写下“未执行”的工具记录。
5. 在同一事务把该工作项未投递的 `work_control_messages.delivered_at` 与 `delivered_session_id` 写上。现有 `ConsumeSteerMessages`（`service/control_commands.go:52`）会插入另一条用户消息 `[Operator Steer Guidance]: [id] text`。闭合路径已经注入指导时，不能再让它插入第二条。
6. 结束本批次工具循环，进入下一模型轮。已执行结果保留。

Worker 构造点在 `engine.go:1058`。当前 `WithBeforeTurnHook` 在第 1067 行调用 `ConsumeSteerMessages`，这只处理“本轮开始前已经到达”的指导。模型已经给出工具批次之后到达的指导，必须走上面的 `DecideBeforeTool`。两条路径共用一个 supervisor。闭合路径标记 `delivered_at` 之后，下一轮的 `ConsumeSteerMessages` 应返回空。

SessionRunner 目前没有 work item ID。增加一个由 Engine 注入的 decider，例如 `WithToolBatchDecider`，不要让 SessionRunner 自己写 steer 行。需要一个只确认投递、不插入 `agent_messages` 的服务方法；建议：

```go
func (s *RunService) AcknowledgeUndeliveredSteers(ctx context.Context, runID, workID, sessionID uuid.UUID) error
```

`delivered_at` 与闭合后的 tool result、单条指导消息必须一起提交。中途失败时，不能留下“调用仍打开且 steer 已投递”，也不能留下“指导已写入对话但 `delivered_at` 为空”以致下轮重复注入。

M1 的 `ToolBatchSupervisor.DecideBeforeTool` 与 `controlplane.MemoryReplan.DecideBeforeTool` 语义相同。合并后只保留一个实例，Controller.Replan 指向它。

## 4. 旧 REST 控制入口改走 Apply

文件：`backend/internal/handler/run.go`。M1 合入且 Agent 1 停止修改该文件的控制接线后，由 Agent 0 改这些入口。Agent 5 没有改这个文件。

| 入口 | 约行 | 当前行为 | 接线要求 |
| --- | --- | --- | --- |
| `PauseRun` | 1113 | 直接 `PauseRun` 再 `manager.Pause` | `pause_run` |
| `ResumeRun` | 1137 | 直接恢复 | `resume_run` |
| `Stop` | 235 | `Stop` 再 `StopAndFinalizeIfIdle` | `stop_run` |
| `SteerWorkItem` | 1161 | 持久化后 `Nudge`，不武装 replan | `steer_work`；Apply 内部会持久化、`NoteSteer`、`Nudge` |
| `KillWorkItem` | 1204 | 先写库再取消 | `kill_work`；Apply 先取消真实 Worker，再写库 |
| `GrantBudget` | 1241 | 人类授权账本 | `grant_budget`，仅人类 principal |
| `StartNewEpoch` | 1313 | Quiesce、StartNewEpoch、Revoke、`startRunExecutor` | `start_epoch`，走第 1 节的 `RecoveryFunc`，不要再执行一遍 |
| `ContinueWorkItem` | 635 | 直接服务调用 | `continue_work` |
| `RetryWorkItem` | 684 | 直接服务调用 | `retry_work` |

适配器读取当前 epoch，把客户端给出的 `expected_epoch` 原样传入。缺省或 0 不改成 1。旧客户端没带 `Idempotency-Key` 时返回 400，不要生成一个会把每次调用都变成新命令或都变成重放的键。

旧 JSON 形状可以在 Apply 成功后继续返回给旧页面，但状态改变必须来自 Apply。新路由返回合同 envelope。Agent 6 消费新路由，不要改 `frontend/api.ts` 来掩盖字段差异。

Kill 的旧顺序是先写库再取消。改接后必须使用 Apply 的顺序：先 `Live.Kill`，再 `RunService.KillWorkItem`。数据库在取消之后失败时，重试是幂等的，因为已经终态的 Kill 返回 nil。

## 5. Agent 2 与 Agent 6 的冻结接缝

- `Workbench.Models` 实现 `ModelCapacity(ctx, runID) (contract.ModelCapacity, error)`。未注入时快照里的限额、在途和排队都是 0。不要在查询层填假数字。
- `MainAgent.Resolver` 使用 Agent 2 的 `contract.RuntimeResolver`。未知别名不能回退到另一个模型。快照只记录别名、协议和 profile。密钥留在服务器别名，不写入 `main_turns.runtime_snapshot`。
- Main 的真实 `Call` 仍使用注入的 `provider.Provider`。生产环境不要默认调用付费模型。
- Agent 6 读取 `GET /api/runs/{id}/workbench` 与 `GET/POST /api/runs/{id}/main/messages`。202 只表示回合已入队。action 消息引用 `command_id`、`model_call_id`、`work_item_id`。`tool_call_id` 列是 UUID，不保存模型的字符串 call id。
- `event_seq` 只分页事件。`last_event_seq` 是本页最后一条，空页时等于请求游标。状态变化即使事件页为空也要出现在同一响应里。
- 同一响应不能同时是终态 Run 和 `running` Epoch。这种行组合会返回 `invalid_state`，没有 `workbench.v1` 快照。

## 6. 迁移 030

文件：

- `backend/migrations/030_main_control.up.sql`
- `backend/migrations/030_main_control.down.sql`

编号是合同预留的 `MigrationMainControl = 30`。服务器现有 `db.RunMigrations` 会在启动时应用。不要改 001–025，不要提交空迁移。

新增表：`execution_commands`、`main_conversations`、`main_turns`、`main_messages`、`main_model_calls`。Run 删除时级联，测试 `TRUNCATE tasks, assets CASCADE` 可以清掉这些行。`main_conversations.session_id` 引用 `agent_sessions`，`ON DELETE RESTRICT`。

主会话锚点由 MainAgent 插入：`agent_sessions.role = mainagent`，`status = completed`，`finish_reason = main_conversation_anchor`，没有 `work_item_id`。Epoch 切换会停止 `running` 会话；锚点保持 completed，避免对话行被拆掉。公开转录在 `main_messages`，不写 Worker 的 `agent_messages`。`main_model_calls` 与 Worker `model_calls` 分开，主回合不消耗 `RunExecutionUsage`，也不增加 Run 的 `max_model_calls`。默认预留来自 `SuggestedExplorationTemplate.MainReservedModelCalls`（8），可由执行输入覆盖。

`execution_commands` 的幂等键是 `(run_id, idempotency_key)`。旧 epoch 或 attempt 不匹配的拒绝发生在写入之前，同一键可以用更正后的内容重试。已接受或已应用的结果按原 hash 重放，不重复取消 Worker。

down 在任一审计表有行时拒绝回滚。生产库不要强制删除这些行。

各 agent 使用自己的测试库。本模块实测库是 `agent_workbench_ma5`。集成库自行迁移，不要把 `TEST_DATABASE_URL` 指到 `agent_workbench_ma5`、`agent_workbench` 或其它 agent 的库。

## 约束

- 一个 Engine。恢复走 `startRunExecutor`，不在聊天里复制 Run。
- 控制效果只经过 `Controller.Apply`。工具只改数据库、不取消已注册 Worker，不算完成。
- 不调用基线 `Manager.SteerWorkItem` 或 `Engine.SteerWorkItem` 作为第二武装。
- Planner 不能 `grant_budget`、`start_epoch` 或扩大范围。Main 不能增加总预算、扩大范围，也不能绕过池容量启动 Worker 或宿主 Shell。
- 终态 Run 的查询只读。明确恢复使用 `start_epoch`、`expected_epoch` 和现有恢复资格，并保留原总结与 `model_calls` 行。
- 不持锁等待模型调用。Main drain 使用独立的进程内互斥和 `0x4D330002`，与 Apply 的 `0x4D330001` 分开。
