# Agent 6：统一工作台前端重构

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

你负责 M4。分支 `codex/ma-workbench`。重点是让用户理解并控制真实多 Agent 执行，不能只美化旧长页面，也不能拿假的4张卡片宣称多Worker可用。

## 范围与依赖

拥有 frontend/src、api/types、样式/组件、Next/package 配置和自己的组件测试。保留 Next/React 现有栈，不做无关框架升级。后端 DTO以 Agent 0 冻结 workbench.v1、Main消息和commands合同为准。可先用显式开发 fixture adapter 做界面，正式交付必须连真实后端。

参考本地 ARTEX 前端的任务会话、方向/trace/资产/发现入口，先定位实际文件；学习用户流程，不逐像素复制。读现有长 Run 页面、TaskForm、ExplorationResources、CheckResultsPanel、api/types 和现有 Playwright。

## 需要交付的工作台

统一 `/workbench`。左侧任务/Run/Planner/方向导航，中部 Main Agent 对话及 Planner 动态/工作方向板，右侧检查器显示所选方向、Attempt/工具/产物/事实/发现。Task/Run/WorkItem有深链接，刷新保持选中，旧路由可以进入新工作台，旧记录仍能看。

### 操作与信息

1. 创建任务以目标、范围、模型配置、能力 profile 和可编辑预算为主；N 输入1–8，默认新任务3，兼容旧1/2。不再有“单/双模式”两套执行逻辑。展示 requested/effective/active/queued，不混淆Worker数量和模型请求并发。
2. 顶部常驻Run状态、Epoch、全局额度和原因；方向卡片显示稳定目标、来源、真实最新步骤、Attempt和局部额度、等待/失败范围。不能把4个model_call画成4个Worker。
3. Main对话是真实服务：用户消息、模型回复、真实action/tool引用、执行中/失败/重试状态。提交pending、202、断线、幂等重发和409冲突可见；不做伪打字/伪进度。
4. 点方向后可以看历史Attempts和trace、分段产物及来源；一键基于目标方向steer/kill/continue/retry。总Pause/Stop、追加授权和新Epoch操作清楚区分，危险范围/预算变更仍走结构化输入。
5. 目标、Fact、Finding、人工confirmed、复测fixed保留独立语义。证据检查器保留原有审核/复测，不让规则审查占整个首页。
6. 用统一查询/轮询源管理小快照，消息/正文按需分页。终态/暂停/复测及切换Run时不串状态；不要将错误catch后统一返回空数组掩盖失败。
7. 首选稳定可用的轮询，不为炫技先造SSE；做SSE时必须cursor重连、快照兜底和去重。
8. 工作台需窄屏布局、加载/空/错误态、可访问标签与焦点；尽量清晰、紧凑、可读。大型图动画和复杂设计系统暂缓。

## 推荐实施拆分

先路由/状态模型/布局 → 冻结DTO adapter → 导航/真实方向板/检查器 → Main对话/真实控制 → 错误恢复/刷新/移动布局 → 真后端联调。为Agent7提供稳定的语义标签/testid，但测试应验证行为而非CSS。

## 必须验证

npm生产构建；真实后端N=4时卡片和计数一致；聊天提交真实控制并定位来源；选中方向看真实Attempt/工具/产物；Kill只影响目标、暂停后仍可Stop；刷新/断线可找回状态；终态与Epoch一致、复测更新可见；旧任务可读。实际浏览器截图来自当次运行。

不修改后端，不伪造字段或静默降级到Mock；API合同不够时把具体缺字段/错误交给0/5，不在前端猜一个成功结果。
