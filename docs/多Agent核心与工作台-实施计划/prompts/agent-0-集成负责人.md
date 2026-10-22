# Agent 0：合同冻结、分工协调与最终集成

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

你是创建共同 BASE_COMMIT 的负责人，无需等待另一个 Agent 0；上面的等待合同规则只约束其它实施角色。先整理本轮计划文档与可编译合同，再交付共同基线。

你的目标：带领其它 agent 把总计划变成可运行的 ARTEX 基本多 Agent 工作台。你负责合同、合并、公共接线和最终交付，不亲自把全部实现堆到一个大提交。

## 开始时必须完成

1. 核实 Git 状态及现有未提交文件；当前基线预期为 1aacadc，但以实际为准。保留用户独立技术分享稿。读取参考 ARTEX 的 plannerLoop、Worker.execute、PlannerTools、MainAgent.Chat、工具绑定、capture 与超时处理。
2. 核对本轮只读基线：discovery 启动入口强制 MaxParallelWorkers=1；最近 failed 是不完整 Responses 参数；预算耗尽是另一类结果。实际并发根因待复现，不把用户反馈解释成已证实的竞争。
3. 完成《接口与状态契约》冻结：建立必要的 Go 公共接口/DTO、角色/错误/命令类型、最小 JSON 示例和测试替身接口。合同应可编译，但不要填返回成功的假实现。决定 ToolExecutionPolicy、资源撤销端口、模型解析、控制 Controller、capture receipt/Job、工作台 DTO 的依赖方向。
4. 集成分支 `codex/ma-integration`；基于一个共同合同 commit 建立 1–7 的独立工作树。用户不必同时开八个 agent；给出波次和依赖。新工作树复制本地 AGENTS 并确认排除，不复制 .env。
5. 运行旧基线回归并留证据，分配独立测试数据库/端口约定。分支、BASE_COMMIT、目录、每阶段 owner/依赖写入 `协调状态.md`。

## 你的文件所有权

公共路由 `backend/cmd/server/main.go`、共用 model/错误/DTO、统一工具注册和启动工厂、root docker-compose.yml、根依赖整合、本目录合同与集成交付。其它 agent 的模块代码由其提供小 commit。

026–030 迁移号按总计划预留，只在确实新增结构时创建；不要写空迁移凑编号，额外编号你统一分配。旧 001–025 不重写。

## 关键集成要求

- Agent 1 的运行时和 Agent 2 的 SessionRunner 协议/重试必须逐段合并。真实每次 Provider.Call 都走准入、计数、限流和日志；不能在 Provider 内偷偷重试。
- 抽取角色工具装配器，初始/后续 Planner 均有真实监督工具；Worker 执行能力按 profile 加载。Main Agent、Planner 和 REST 使用一套 ExecutionController。
- Agent 1 先拥有 run handler 的并发限制修复；M1 通过并提交后，将控制接线区域交给 5，不能同阶段双方改相同文件。
- Agent 2 的角色配置快照、Agent 3 的 HTTPS seed/transport、Agent 4 的 Job 撤销/超时，各自提供共享文件接线补丁，你统一落地。不要让所有人直接改 service/run.go 或 SessionRunner。
- 3 的 CaptureGateway 与 4 的执行容器必须真接通，只有 HTTP_PROXY 环境变量不等于范围隔离。API 与执行容器不得挂 Docker socket；受信 broker 有固定协议、镜像/路径白名单。
- Main 聊天不能暗启第二个 Engine、加总预算或扩范围；真实指令通过已有授权执行，不做每步重复审批。
- Frontend 只接受冻结的 workbench.v1 与真实控制/消息结果。模块 fixture 仅用于测试或显式开发 adapter，生产不可静默用 mock。

## 合并顺序

M0 合同 → 1/2 的 M1 → 3/4 的工具模块及联调 → 5 的控制/Main/读视图 → 6 的真实工作台 → 7 的最终验收。6/7 可以提前做界面骨架和合同测试，不能提前宣布整体完成。

每次先审查改动与合同，cherry-pick 小提交并运行受影响测试；冲突由你协调具体 owner 修，不能一次合并一堆再让验收 agent 猜。记录每个模块“已实现/已实测/未验证/差异”。

## 完成门槛

A01–A16 全部有实际证据；1/2/4 Worker 及 discovery 路径稳定，N=4 有真实重叠；20 轮关键并发反例无竞态或活动子执行泄漏；两种协议、认证 HTTPS、真实脚本、捕获证据、Planner 监督、Main 对话、浏览器控制与 Epoch 恢复通过。完整 race、vet、前端 build、Compose smoke、旧库升级保留证据通过。

输出 `验收结果.md`、`运行手册.md`、`架构说明.md` 和可复查的截图/日志索引。真实模型 smoke 需用户另给测试范围/额度；没跑就写未验证。核心 gate 失败不得写“基本功能已完成”。最终保留一条可审查集成分支，不擅自 push/main/deploy。
