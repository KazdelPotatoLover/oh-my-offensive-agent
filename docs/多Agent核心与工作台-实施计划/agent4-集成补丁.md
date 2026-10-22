# Agent 4 集成补丁（交给 Agent 0）

本文件只描述接线。Agent 4 没有修改根 Compose、Engine、Manager、SessionRunner、默认 profile 或 Agent 3 的捕获网关。

## 已在本分支实现

- 迁移 `backend/migrations/029_execution_jobs.up.sql`。子产物走 `execution_job_artifacts`，`artifacts.source_tool_call_id` 的唯一约束保持不变。Down 在 `execution_jobs`、`execution_job_artifacts`、`execution_job_traffic` 或 `execution_files` 仍有行时拒绝。
- `backend/internal/execution`：`Executor` 实现 `contract.JobExecutor` 和 `contract.ResourceRevoker`。`Revoke` 只处理 `RevokeJob` 与 `RevokeAttempt`，其它 kind 返回错误，因此它不能单独充当唯一 revoker。
- Broker 是唯一打开 Docker socket 的进程，协议 `execution.broker.v1`。它不执行宿主机 shell。
- 能力工厂 `run_command`、`write_file`、`read_file`、`inspect_command` 由 `execution.RegisterAdapters` 显式注册，不在 `init` 中注册。
- 执行镜像 `oma-executor:ma4`。构建命令：

```bash
docker build -f deploy/execution/Dockerfile -t oma-executor:ma4 backend
```

- 部署片段：`deploy/execution/compose.fragment.yml`、`deploy/execution/broker.Dockerfile`。

## 必须由 0 接线

1. SessionRunner 逐工具超时。现有 `session_runner.go` 约 702 行对所有工具使用 `budget.ToolTimeoutSeconds`（默认 5 秒）。命令工具应使用请求时长，再经 `execution.ClampCommandTimeout` 被 Run/Attempt 剩余时间和调用方 deadline 夹逼。不要把 5 秒当成命令上限，也不要放宽 Run 全局 deadline。执行器不会把这个等待再延长成命令时间。
2. Settlement 阶段调用 `execution.WithPhase(ctx, execution.PhaseSettlement)`。位置在 SessionRunner 将 `inSettlement` 设为 true 之后（约 255、502、588 行）。不要把 `run_command` 或 `write_file` 加入 `orchestration/budget.go` 的 settlement 安全名单。`read_file` 与 `inspect_command` 可以加入，它们只读已有材料。
3. 工具执行前调用 `execution.WithSourceToolCall(ctx, toolCallID)` 和 `execution.WithEpoch(ctx, epoch)`，让产物和准入使用当前 Attempt 的 epoch。
4. `cmd/server/main.go` 在服务请求前：
   - 用环境变量中的 broker URL 和 token 构造 `execution.NewExecutor`。
   - 注入 Agent 3 的 `contract.CaptureGateway`。网关为空时，离线 Python 仍可运行，curl 失败关闭。
   - 调用 `execution.RegisterAdapters(executor)`。不要改 `model.DefaultProfiles`。
   - 用 `context.WithoutCancel` 加 `contract.CommandCleanupWindow` 调用 `executor.Reconcile`。Reconcile 先让 broker 停掉本 broker 标签下的容器，再把未终态行标成 `outcome_unknown`。已终态行不会被改写。
5. Manager 的 Stop、KillWorkItem、RevokeEpoch、Shutdown 需要扇入 `Executor.Revoke`。清理上下文同样使用 `context.WithoutCancel` 和 `contract.CommandCleanupWindow`。不要用本 revoker 替换 HTTP 或模型等待的 revoker。
6. 把 `deploy/execution/compose.fragment.yml` 并入根 Compose，不要覆盖现有服务。API 容器和执行容器不挂 Docker socket，不 privileged，不用 host network / host PID。Broker 是唯一挂 socket 的服务。片段里的 `user: "0:0"` 只属于 broker，因为宿主 socket 通常是 `root:docker` 660；不要把这个用户复制到 API 或执行容器。执行容器由 broker 强制为 `10001:10001`。
7. 预构建并只允许镜像 `oma-executor:ma4`。模型不能提交镜像名、docker 参数、挂载路径或 host PID/network。

## 建议的 profile 片段

不要改默认 profile。启用命令能力的 profile 显式列出：

```json
{"name":"run_command","version":"1"},
{"name":"write_file","version":"1"},
{"name":"read_file","version":"1"},
{"name":"inspect_command","version":"1"}
```

## 证据边界

`run_command` 的工具输出带 `empirical: false` 和 `evidence_class: execution_result`。非零退出码是可分析的执行结果，不是整个 Run 失败。stdout 和 exit code 不能充当 empirical 来源。可信 HTTP 证据只来自注入的 CaptureGateway 写下的 receipt。本分支用 `StandInGateway` 做测试替身，生产必须注入 Agent 3 的网关。

## 有意适配

- 工作空间按稳定 WorkItem 保存。新 Attempt 从 `execution_files` 恢复文件，不复活旧容器。
- 出口隔离使用每个 Job 的 internal 网络和只拨捕获回调的代理。仅设置 `HTTP_PROXY` 不算完成。
- 原始 TCP、raw socket 和 Kali 镜像不在本轮。
