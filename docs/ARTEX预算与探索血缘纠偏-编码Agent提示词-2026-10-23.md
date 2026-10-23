# 交给编码 Agent 的提示词

请直接实施以下修复，不要只回复分析或再写一个 plan。

实现仓库：`/home/hjdssj/project/personal/oh-my-offensive-agent`
参考仓库：`/home/hjdssj/project/personal/ARTEX`（只读）

先阅读本地 `AGENTS.md`，然后完整阅读：
`docs/ARTEX预算与探索血缘纠偏-修复计划-2026-10-23.md`

这份计划针对用户刚指出的两处偏离，优先于此前“创建任务简化与默认预算”里默认 240/600/30 分钟的方案。不要退回“折叠参数 + 放大预算”或“前端自动补连线”的做法。

本轮完整目标：

1. 新任务执行策略贴近 ARTEX：默认 3 Worker；任务总时长可选、默认不限；默认不以模型/工具总次数、规划总轮数和方向总数任意封顶；Worker 单轮默认 1200 秒、MaxTurns 默认不限，局部耗尽进入独立收尾，默认收尾 10 轮，再通知 Planner 判断。专家显式硬上限仍可用。
2. 为新策略增加明确版本和集中解析；新版本的“省略=继承、显式 0=不限”不能改变旧 Run 的零值语义。贯通 BudgetManager、WorkerBudgetGate、SessionRunner、数据库侧准入、调度、deadline timer、收尾、Main、恢复与 grants。不能只改配置文件，也不能用巨大的整数假装不限。新 Main 不再被整个 Run 累计 8 次对话额度卡死。
3. 持久化唯一任务起点（参考 fact/origin）。初始用户 goal 和 SetGoals 写 origin→goal 的 spawns；没有显式知识来源的初始意图写 origin→intent 的 derived_from；有真实 fact/finding 来源则按实际来源保存；保留 intent→fact，并补齐可信执行上下文中的 intent→finding。
4. 提供历史图幂等回填，使现有 Run 也有可追溯起点与初始关系，不修改原节点 ID、证据、预算、Epoch 或状态。不猜测目标树和 Worker 成果归属，不制造 proves。
5. 保留两字段创建表单和默认服务器模型。前端明确显示不限时/有限时长和真实边；旧有界 Run 仍显示原预算。维护手机端滚动及读取失败提示。

开工时记录当前工作区：已有本轮表单/预算未提交代码，并有其他 agent 的 MCP、提示词和镜像改动。保留这些修改，协调重叠文件；禁止 reset 或整文件覆盖。用户技术分享文稿不改。本地 AGENTS.md/.env 不上传；迁移编号按最新目录分配，不覆盖未提交的 032 MCP 迁移。

请先核对计划列出的 ARTEX 源码，特别是：
- `server/server.go:agentMaxTurns/agentRunSeconds`
- `agent/worker.go`、`agent/wrapup.go`、`agent/taskclock.go`
- `agent/tools.go:addOneIntent/addOneGoal`
- `db/exploration.go:OriginFactID`

不要把 MaxTurns 当工具次数；不要把起点当已验证漏洞证据；不要自动续租所有耗尽方向；不要借此重做整个 Agent 框架、图数据库或完整设置中心。

按计划的 A/B/C 阶段和验收矩阵完成工作。使用独立 PostgreSQL 测试库和本地模型替身/fixture，保留有界配额、越权和证明失效反例。必须覆盖旧 Run 不扩权、新任务没有隐藏小额度兜底、Main 超过 8 次交互、局部多轮收尾、可停止、首次派工即有真实边、两 Worker 来源正确、历史回填两次无重复。无需 20 轮压力测试，不自动对用户网站发请求。

完成后交付：

- 具体改动与 ARTEX 对应关系；
- 测试命令和结果；
- 新旧策略的实际 JSON 示例；
- 历史图回填与本地更新命令；
- 当前运行服务是否已更新、剩余限制；
- 仅包含你负责内容的清晰提交边界。

没有另外授权时不要推送 GitHub 或重启正在执行用户任务的服务。发现难点先在现有架构内推进，不通过改大默认额度、吞错、伪造关系或删除测试来绕过。
