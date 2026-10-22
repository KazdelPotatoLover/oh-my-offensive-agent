# Agent 1：稳定 N Worker 调度与故障隔离

实现仓库：`/home/hjdssj/project/personal/oh-my-offensive-agent`。参考仓库：`/home/hjdssj/project/personal/ARTEX`，只读。不要在 ARTEX 中改业务代码。

上述实现路径用于定位主库；实际编辑和本地提交必须在你分配到的独立工作树中完成。

你接手的是已存在的 Go + PostgreSQL + Next/React 工程，不是从零重做。先读取本地 AGENTS.md、`docs/多Agent核心与工作台-实施计划/README.md`、`接口与状态契约.md`、`并发与失败排查基线.md`，再读取分配模块及对应 ARTEX 源码。旧 P0–P7 是基础，不代表本轮核心多 Agent 已可用。

遵守这些规则：
- 使用 Agent 0 提供的 BASE_COMMIT 和自己的 `codex/ma-*` 分支/工作树。尚未提供共同合同 commit 时，可以只读分析和准备测试，不自行猜接口实现。
- 本地 AGENTS.md 在工作树中保留并排除；不要上传 .env、凭据或用户技术分享草稿。
- 只改分配文件；公共接线和他人文件的必要改动列成补丁清单交给 0。不要复制整个目录覆盖别人的实现，不直接 push/main/deploy。
- 不增加第二套 Engine，不用串行兜底掩盖并发问题，不用假状态/固定页面/模型文案冒充证据，不删掉预算、范围、历史与审核保护。
- 数据库测试会 TRUNCATE，只在你新建的独立测试库运行。不得使用开发库 agent_workbench，也不要多个 agent 共享一个 TEST_DATABASE_URL。端口使用 httptest/临时分配或合同预留端口。
- 不默认调用付费模型、不接触外部靶场、不重启现有开发服务。用本地协议替身、真实 fixture 和隔离容器验证。
- 先实现并运行失败用例，再修复、验证相关反例，按小提交交付。有接口冲突通知 0；不自行启动额外 agent。
- 交付必须列出：commit、改动文件、实际测试命令/结果、未验证项、有意适配差异、集成补丁及迁移要求。不能仅提交 plan 或一句“完成”。

你负责最高优先级 M1：让 N Worker 真正运行且局部故障不会误杀整个 Run。分支 `codex/ma-runtime`。

## 对照读取

ARTEX `server/engine.go:plannerLoop`、worker loop、runIntent、Pause/Resume/KillWork；`server/engine_timeout.go`；`agent/worker.go` 的恢复和 settlement。本项目 engine.go、budget.go、manager.go、service/agent.go 的认领/结束/续派、admission/control/cancellation，以及旧并发测试。

## 所有权

orchestration engine/budget 与新增 scheduler/coordinator，worker manager，service 运行时资格/认领/依赖/预算片段；M1 阶段的 handler/run.go 并发修复；必要的 026 迁移及你命名的 ma_runtime_* 测试。不要改 Provider 协议、工具网络实现、前端或 Main Agent。SessionRunner 的 gate/暂停/工具前边界变更与 2 通过 0 逐段集成。

## 执行步骤

1. 写能失败的全链路测试：创建 discovery Run 请求 N=4，断言原请求、快照、effective 和实际峰值一致。当前 handler 强制串行必须被测试捕获；用四个不同方向及屏障观测重叠，不只数 completed。
2. 追踪 UI 请求到 service、Handler、Engine 的并发配置，范围 1–8。默认新任务 3，旧快照缺失保留 1；不允许静默降成 1 或再创建 DualExecutor。
3. 将现有 Engine 整理为单 Planner、独立 Dispatcher、有界 Worker 池与统一 Run 协调器。Planner 调模型期间不能阻止 Dispatcher 领取刚提交的新方向；锁内不调模型/网络。保持持久 changefeed、debounce、Todo 与 wait，不能退回整批完成才计划。
4. 同 WorkItem 一个活动 Attempt、池槽位到最后持久化完成才释放。认领、去重、依赖阻塞恢复、旧 Epoch/Attempt 迟到均有事务和约束。暂停/停止时先撤销新准入并停妥 Dispatcher，再有界 drain，避免 WaitGroup Add/Wait 竞争。
5. Worker 局部额度进入 settlement/exhausted；protocol/provider/tool/refusal 等最终局部失败保留原原因，其它 Worker 继续，由 Planner 有界选择重试。持久化损坏/Run 停止/硬总预算等才协调全局收尾。
6. 与 2 接入角色 RuntimeResolver 和共享模型请求限流；N=4、模型请求并发=2 时显示等待，不误报失败，取消能唤醒排队。所有请求/工具严格共享总预算，续派/Epoch 不清用量。
7. 为 5 提供 Executor 控制及安全 pre-tool replan 端口。Steer 到达时，未执行的旧工具批次需正确闭合后让模型重新评估，不能注入指导后照常执行旧动作；实际实现和原生协议配合 2/0 集成。
8. 提供 Job/HTTP 撤销资源接线供 3/4 使用，Kill 只取消目标方向，Stop/Epoch 撤销全部旧资源。不要把取消原因全部变成 internal_error。
9. 根据本地实测提供显式预算模板建议，包含 Planner 监督余量。不得只调大预算或加 sleep 把失败藏起来。

## 必须实际验证

- 旧 DualWorker、P3 事实唤醒、定向 Kill、Responses 隔离、续跑/Epoch 测试保留。
- N=1/2/4 + discovery 真重叠；峰值不超 N、无重复认领。
- A 在途写 Fact，Planner 派 B，B 在 A 结束前开始。
- 一个失败、一个耗尽、一个慢调用时另一个继续；无工作时 wait、无事件风暴。
- 并发总预算边界、暂停准入、Stop/Kill、旧 Epoch 返回、依赖继续后解阻塞。
- 关键测试重复 20 次并跑 race，交付实际最大重叠、关键事件顺序和无泄漏断言。测试库只属于你。

交付时给出根因与确定证据，区分“代码已确认串行覆盖”和“真正复现的并发故障”。M1 合并后把 run handler 控制区域交接给 5，不再越界修改。
