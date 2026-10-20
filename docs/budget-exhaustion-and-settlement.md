# 预算耗尽、页面观察与收尾

本次设计复用 `BudgetManager` 和 `FinalizeRunWrapup`，不改变历史 Run，不引入自动重试或新的调度框架。

## ARTEX 已有行为

- `agent/tools.go:recordFact/recordOneFact` 要求一次探索的状态、标题、响应特征等尽量汇总为一条事实；不同结论可以放进 `facts`。它的批量返回同序 `ids` 和按索引的 `errors`，允许部分成功。
- `agent/worker.go` 使用 harness 的 `MaxTurns`、`MaxDuration`、`Settlement`。`MaxTurns` 是模型/agent 轮数，不是工具调用次数；一轮可以请求多个工具。
- `agent/wrapup.go` 给收尾单独的轮数及提示词，并由代码决定禁用的工具（Worker 禁用 Bash），用于停止探索后写回已有结果。
- `server/engine.go` 将 Worker 的 `ReasonMaxTurns`、`ReasonTimeout` 归为 `exhausted`，执行异常另行处理；`NotifyDone` 把结果变化交给 Planner 决策。
- `server/engine_timeout.go` 先停止领取新工作，等待有界 drain，再做任务终局判定；终态写入有守卫。ARTEX 的任务超时收尾与单次 Worker 耗尽是不同层次。

## 本项目设计和实现

### 页面观察

优先通过原有 `record_observation` 保存一条 `page_observation`，其 `data` 包含 `status_code`、`title`、`response_features`、`content_summary`。旧类型和单条调用及其返回结构继续兼容，字段仍存于可查询的 JSONB，`source_tool_call_id` 继续引用原始成功读取证据。

新增 `record_observation_batch` 仅用于不同结论或不同来源，每批 1–16 条，计一次工具调用。返回 `{"status":"recorded","count":N,"observation_ids":[...]}`，ID 按输入顺序排列。不同于 ARTEX 的部分成功，本项目在同一 PostgreSQL 事务中先逐条校验 Run、Worker Session、Attempt、成功的源工具调用、源资产及 Run/工作项范围，再写观察和事件；任何失败全部回滚。单条与批量复用来源校验，保留 legacy 范围策略。

### 状态

| 层次 | 状态 | 含义 |
| --- | --- | --- |
| Worker 工作项 / Attempt / Session | `exhausted` | 预算不足，工作尚未完成；已保存证据保留 |
| Run 总模型/工具额度 | `stopped`，原因 `model_calls_exhausted` / `tool_calls_exhausted` | 停止探索，保存部分成果；UI 显示“预算耗尽 · 部分完成” |
| Run 真实错误 | `failed` | 协议、数据库、provider 或程序错误，不降级为部分完成 |

迁移 012 扩展三个子状态约束，不增加 Run 枚举。依赖判断、查询、前端、恢复和统一终态清理均识别 `exhausted`。Run 额度耗尽时，运行中的工作标为 `exhausted`，尚未启动/阻塞项标为 `cancelled`，完成项保留。清理和迟到结果仍使用 Run 行锁与已有终态守卫。

当前 Engine 没有独立 Worker 配额；不为此新增配置或重建调度。现有独立 Session 路径的局部耗尽只结束该工作，不直接结束父 Run；局部耗尽导致的阻塞可以交回 Planner，真正失败的工作仍走失败路径。Planner 可以提交有界后续工作，不会自动重放外部操作。

真实 Worker 错误优先于同时到达的额度边界：例如最后一个探索模型请求返回协议错误，Run 仍以 `failed + protocol_error` 结束，不被归为部分完成。用户停止和原运行截止时间继续由统一收尾判定。

### 探索与收尾预算

继续使用集中、并发安全的 Run 模型/工具硬上限。Engine 启动时计入同一 Run 已持久化的请求，原始 `started_at + run_timeout_seconds` 决定截止时间，不能重建 Engine 来刷新预算或时间。

HTTP 创建入口的 discovery mock 路径也使用原始记录额度；移除该入口原先把模型/工具额度自动提高至 80 的行为。

总模型额度大于 1 时，在其中预留 1 次模型总结；小预算不预留，直接生成程序化报告。探索每一模型请求（包括工具调用后的后续轮次）和每一工具调用分别计数。Planner 能看到剩余总模型额度、探索模型额度、工具额度和时间。

本次收尾**不写回新事实**：工具注册表为空，工具 gate 拒绝所有工具，预算管理器关闭探索 admission。因此不预留收尾工具，也没有“免费工具调用”。只允许一次模型请求整理已持久化证据，不允许访问新页面、提交新计划或继续工具循环；失败、拒绝、要求工具时保留程序化报告。

### 时间与可靠性

- `summary_timeout_seconds` 默认 30 秒（旧配置缺省时回退模型时限），取其与原 Run 剩余时间的较小值；不再固定为 5 秒，并独立于探索请求的 `model_timeout_seconds`。
- Worker drain 延续原有 5 秒 grace。它和模型总结时限、数据库清理时限是三个不同的窗口。
- `cleanup_timeout_seconds` 默认 10 秒；数据库证据快照与最终清理使用独立、有界的后台 context。总结耗时不会吃掉最终事务的时间。
- 用户停止后不再发起总结；运行中总结收到取消即退回程序化结果。总结超时、不可用、拒绝、额度不足都不丢弃已有证据。即使 provider 忽略取消，最终事务也不会等待它无限返回；迟到结果不能复活 Run。

### 覆盖与报告

发现数 = Run 登记资产（包含种子）；规划数 = 关联到工作项的不同资产；访问数 = 有成功 `get_endpoint` / `read_sample` 证据的不同资产，独立于观察数和完成项数。HTTP 错误状态响应也是一次访问；网络工具失败不算成功访问。重复读取只计一个资产。

Planner 和程序化报告使用同一覆盖查询。报告分别列出发现、规划、访问、未访问、完成工作、观察与工具失败，并记录配置范围/发现限制。正常结束也附加这份库存。所有数字标为终态清理前快照；未启动项和中断项不会继续显示为等待执行。完成工作不代表完整安全覆盖，工具次数也不代表页面数。

## 兼容与限制

需要先升级迁移 012 再运行新服务；旧客户端须接受子状态 `exhausted`。Run 枚举不变，但仅以 `failed` 识别额度耗尽的消费者须改读 `stopped + finish_reason`。资产历史接口新增可选 `finish_reason`。不回写旧 Run 的状态或报告。

若已产生 `exhausted` 行，012 降级会明确拒绝，防止为适配旧约束篡改历史结果；无此类行时可回滚。状态、证据和终态可追溯性优先于兼容旧版本的错误分类。

聚合由明确的工具 schema、Worker contract 和测试约束模型使用；不会自动把可能含不同结论的历史观察合并。数据库不可用时不能保证即时提交，已有终态守卫和启动恢复仍是兜底。未新增局部时间配额、事实写回型 Settlement 或自动重试。

验证使用独立测试库、fake provider、保存的 HTTP 响应及本地 httptest fixture；不调用真实模型或靶场。覆盖 6 秒总结成功、总结超时/拒绝/不可用、总时限、用户停止、小预算、局部与全局耗尽、来源校验和原子回滚、并发硬上限、终态清理、迟到结果及真实错误分类。

## 实际验证结果

2026-10-20 验证通过：

- 全量回归 `go test -count=1 -timeout 180s ./...`；最后的边界修正后再运行全量 `go test -race -count=1 -timeout 240s ./...`，全部通过（集成测试约 91 秒）。
- `go vet ./...`、前端 `npm run build`（包含 lint 和类型检查）、`git diff --check` 均通过。
- 新隔离库完成全套迁移；012 的 up/down 和存在 `exhausted` 记录时拒绝降级均已测试。

| 场景 | 主要测试 |
| --- | --- |
| 工具全成功但总工具额度耗尽，证据、合并字段和覆盖保留 | `TestBudgetToolExhaustion_AllToolsSucceedAndEvidenceSurvives` |
| 批量来源逐条校验、数量上限、插入中途失败整体回滚 | `TestObservationBatch_IsAtomicAndTraceable` |
| Worker 局部耗尽，父 Run 仍可由 Planner 安排有界后续 | `TestWorkerLocalExhaustion_LeavesRunActiveForPlanner` |
| 总结耗时 6 秒成功；超时、总时限、停止、拒绝、要求工具、小预算回退 | `TestBudgetSummary_DeadlineConfigurationAndCancellation` |
| 总结不可用回退，最后允许的探索调用完成后才收尾 | `TestLifecycle_LastAllowedCallAndSummaryFallback` |
| 并发模型、工具和收尾申请受原总硬上限约束 | `TestBudgetConcurrentToolsAndSettlementRespectBothHardCaps` |
| 终态无活动子任务，迟到结果不能复活状态，重启保留停止意图 | `TestLifecycle_*`、`TestCancellation_*` |
| 最后探索请求发生协议/provider 错误仍失败；mock 入口不提额 | `TestWorkerErrorAtExplorationBoundary_RemainsFailed`、`TestDiscoveryHTTPEntry_UsesRecordedHardCapsForMock` |

本文初次实现验证使用 `agent_workbench_codex_test` 和 `agent_workbench_codex_migration_20261020`；后续提交审查使用独立临时测试库，结果见《第八阶段-检查结果与候选发现》。诊断 Run 的历史状态、观察和报告未回写，未自动部署。
