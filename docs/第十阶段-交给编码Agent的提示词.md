# 第十阶段编码 Agent 提示词（ARTEX 对齐版）

> 本文件是运行时子任务提示词。用户已要求完整纠偏旧路线，执行整轮改造请使用 [ARTEX 对齐改造提示词](ARTEX对齐改造-交给编码Agent的提示词.md) 及其 P0–P7 总计划，不能只做本文件后宣称整轮完成。

此版本取代此前“创建关联新 Run”的提示词。复制下面正文。

---

请在 `/home/hjdssj/project/personal/oh-my-offensive-agent` 实现第十阶段“分层预算与可恢复执行”。用户目标是学习 ARTEX 核心架构，不接受仅做最小续跑按钮或通过新建 Run 绕开会话恢复。

先读适用 AGENTS.md（如有）、`docs/第十阶段-分层预算与可恢复执行.md`，以及相邻 `/home/hjdssj/project/personal/ARTEX` 中该文档列出的 Worker、Planner、rerun、ReopenIntent、settlement、timeout 源码。不要改 ARTEX。先检查现状，保留他人改动；旧版“关联子 Run”方案已撤销。

请完成以下机制，不能只加字段、按钮或提示词：

1. 分层预算：Run 总次数和时间是共享硬上限；每个 Worker Attempt、每轮 Planner 有局部额度及有界 settlement。全局与局部准入同时生效，用类型化原因分清作用域。局部耗尽只结束本轮并通知 Planner，不能终止整个 Run；所有真实调用与收尾计入总量。
2. 稳定工作：WorkItem 保持 ID、目标、资产、依赖及成果；继续创建新 Attempt/执行 Session，保留旧终态。用事务、CAS、唯一活动约束阻止重复认领和迟到覆盖。exhausted 不满足依赖，上游完成后只解除系统依赖阻塞。
3. 稳定 Conversation：跨 Attempt 加载同一工作原始历史，只写本轮消息增量。Chat Completions 和 Responses 都支持，保留 NativePayload、工具配对和来源所有权。成功工具不重做；多工具批次未执行/未知结果明确记录并闭合协议，不伪造成功、不自动重放副作用。数据库工具保护提交成功但响应丢失窗口。
4. Planner 续派：每轮新 Session + 当前态势/触发原因 + 跨轮共享 PlannerTodo，不照搬 Worker 的完整对话恢复。ARTEX TodoStore 为进程内共享；本工程落 PostgreSQL 以支持重启时明确标为增强。提供 trace/output 查询及结构化继续操作，依据真实进度选择继续、重试、换方向或等待，原因持久化，Worker 不能自续额。持久化状态与合并通知驱动执行，增加有界续派和无进展限制，禁止换 key 无限绕过。
5. 人工控制：活动 Run 内继续工作/故障重试复用同一事务，不启动第二个 Engine。completed、主动停止和故障重试分清。页面展示 Attempt 历史、恢复来源、局部/全局预算、续派原因及真实进度。
6. 整体恢复：预算耗尽/关机 interrupted 后保留原 Run/WorkItem/Conversation，引入 execution epoch 和不可变 budget_grants，累计用量不归零，原授权与历轮终态/报告可追溯。所有执行写入、收尾与 Manager 生命周期做 epoch 隔离。只有用户能追加授权；幂等、并发、单活动执行、启动失败及重启均处理，不用新子 Run 替代。
7. 同 Run 复用已保存 HTTP 证据，保留证据来源校验和人工审核保护，不复制假工具记录，不强迫因续跑重复采集。

准确区分 ARTEX 原机制与本项目适配：ARTEX 没有被证实采用相同全局次数硬上限、epoch 或授权账本；恢复 transcript 不是恢复工具进程，也不保证所有外部副作用恰好一次。

按设计 10A→10B→10C→10D 分步完成、分步验证。四部分都属本阶段交付。若遇到实际阻碍，明确已完成子阶段及缺口，不能自行降级到旧方案或遗漏其余部分后宣称完成。按职责拆文件，复用统一预算/准入/停止/收尾机制，不复制执行引擎。

验收按文档第 11 节，重点验证：局部耗尽不拖死 Run、多 Attempt 原历史恢复、Responses 协议完整、HTTP 不重复请求、依赖解阻塞、无进展止损、并发不超额、旧 Attempt/epoch 迟到拒绝、停止/重启安全、总授权追加及历轮历史。

专项 fake provider 要稳定驱动真实 Engine/服务/fixture 链路，不用手工改成功状态代替。数据库测试只用新建独立 TEST_DATABASE_URL，测试会清表，绝不能连接开发库。不调用真实模型和 192.168.77.88 靶场。

运行相关 race/回归、go vet、前端生产构建、git diff --check；完成浏览器工作继续/预算追加/历史/停止验收。新增 `docs/验收/第十阶段/验收结果与流程讲解.md`，记录实际结果、ARTEX 对应关系和有意差异，未执行验证明确标注。

默认不部署、不推送、不自动提交、不改写历史事实；迁移兼容旧数据，不伪造历史授权，不加入密钥。最后说明完成哪些子阶段、真实验证、如何验收和剩余限制。
