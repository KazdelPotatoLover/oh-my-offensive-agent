# Agent 5：Planner 主动监督、Main Agent 和工作台 API

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

你负责 M3。分支 `codex/ma-control`。依赖 Agent 1 的 M1 和 Agent 2 的角色解析合同；M1 没合入前先读代码和准备新文件/测试，不与 1 同时改 run handler。

## 对照读取

ARTEX `agent/tools.go:PlannerTools/killWorkTool/steerWorkTool`、`agent/mainagent.go:Chat`、`server/engine.go` 的监督/取消。本项目 service/control/control_commands/agent 的继续与重试、Manager 的 ControllableExecutor、初始/后续 Planner registry、目标证明和产物查询。

## 所有权

新 controlplane Controller、MainConversation/MainAgent、workbench 只读聚合服务与 handler、监督 tools 工厂、必要030迁移、ma_control_* 测试。M1 交接后接管 handler/run.go 的控制入口接线；公共路由挂载/Engine工具注册/启动工厂移出handler的共享部分交 0，不再自己实现一套状态 SQL。

## 执行步骤

1. 定义并实现共享 ExecutionController.Apply，校验 principal/role、run/epoch/attempt、状态、幂等和授权；组合现有 Service 与 Manager/ExecutorFactory。REST、Planner 和 Main 使用同一路径，禁止模型工具只改数据库却没取消真实 Worker。
2. 为 Planner 增加真实 steer_work、kill_work，并复用 continue/retry、trace/产物查询。初始和后续规划统一装配。作用对象按 ID/预期Attempt定位，错误有类型、动作有审计和投递结果。
3. Steer 在下一工具动作前令 Worker 重新评估。与 1/2 采用冻结的 replan 端口：已执行结果保留，未执行的旧工具批次正确闭合，再注入指导进入下一模型轮。不能收到指导后仍执行原动作，也不能破坏 Responses/Chat 的调用配对。
4. Main Agent 建真实持久会话，独立 role/session/turn/预算。支持解释当前进度、引用 trace/证据、添加Hint/方向、纠偏/终止及用户明确要求的暂停/继续/停止。多用户消息顺序和重复提交幂等；不混写 Worker 会话，不创建另一套 Planner。
5. 主对话仅开放必要态势/控制工具，不给它绕过池容量的任意 Worker 启动或直接宿主 Shell。Planner 不能 grant_budget/start_epoch/扩范围。Main 对明确授权内指令直接操作，预算或范围变更复用现有结构化授权，不做泛化每步审批。
6. 终态 Run 的状态查询只读；用户明确恢复时走现有 Epoch/Controller 通道、expected_epoch 和恢复资格，保留原总结及用量。不能在聊天里偷偷重置 Run 或复制新 Run 冒充恢复。
7. 按合同提供 main/messages POST/GET 与 workbench.v1 聚合读视图。202 的消息提交不伪装操作已完成，action引用真实 command_id、session/tool/work IDs；读取小快照，大正文分页按需加载。
8. 读视图配置并发/实际活跃/排队、模型限流、Planner wait理由、方向/Attempt/最新步骤、错误和允许动作。保证同一响应终态一致；event_seq 只用于事件分页，不能漏掉其它状态更新。
9. 与 2 接角色模型解析，密钥只引用服务器别名。与 6 用冻结 fixture/API 联调，不直接编辑 frontend/api.ts 来隐藏接口问题。

## 必须验收

Planner 根据在途 trace 纠偏A/终止B而C继续；Main真正查询并控制C；重复命令幂等、旧epoch/attempt拒绝、控制与消息有审计；Main不能加总预算/扩范围/多启Engine；终态只读查询、明确恢复仍保留历史；上下游两种协议下指挥链正确；聚合视图不残留“Run completed但Epoch running”。

交付真实动作序列和回归用例，不接受仅返回聊天文本/模拟工具动画。角色模型可以相同；丰富agent市场、复杂约束编辑器暂缓。
