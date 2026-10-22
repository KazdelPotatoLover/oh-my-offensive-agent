# Agent 2：模型协议恢复、会话隔离与角色运行配置

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

你负责 M1 的模型侧。分支 `codex/ma-provider`。目标是多角色/多 Worker 在真实兼容 API 行为下可诊断、有界恢复，不把协议问题当并发问题，也不伪造可执行工具调用。

## 先读

本项目 provider/config/openai/responses/provider/compaction、SessionRunner 的 model 调用和协议分支、responses_test、会话隔离/预算测试。参考 ARTEX 的角色配置实际调用、AugmentTools、Worker/Planner 的 Provider 与 Native/SDK 会话使用；先定位定义再描述行为，不凭 README 猜。

## 所有权与接口

Provider 包、角色 RuntimeResolver、新模型调用诊断/策略、SessionRunner 的协议与调用重试片段、你的 ma_provider_* 测试；确需数据库结构时使用预留 027。不要直接改 Engine/Manager、Main handler 或前端。SessionRunner 与 1 的 gate/hook 按区域由 0 合并。角色快照进入 service/run.go 的接线交给 0。

## 实现顺序

1. 用本地 HTTP 模型替身复现：Responses 完整外层状态但 function arguments 是不完整 JSON / function 状态非 completed。当前错误摘要为 incomplete function arguments or status；不要把它误归为双 Worker 竞争。
2. 分清合法兼容形式与不完整输出。仅对语义明确的可选字段兼容；无法证明完整时不执行工具、不猜 call_id、不自动补 JSON，不把原失败改成功。保留脱敏诊断/原始响应的受控引用。
3. 建 typed 错误：HTTP status、协议类别、是否可重试、Retry-After、request ID、源 model_call ID。上游 429/5xx/临时超时与坏 schema、refusal、截断分开。错误不带密钥、Authorization、私密 prompt 原文。
4. 实现有界恢复/退避，统一由 SessionRunner 的真实调用循环调度。每一次 Provider.Call 都经过限流、共享 gate、模型次数记账和调用记录，禁止 Provider 内藏多次重试。等待限流时可取消；用户停止/Run deadline 优先，不能 sleep 到过期还发下一次请求。
5. 对可能已执行过工具的情形，只恢复已保存的结果和原始协议历史；不自动重放外部动作。无完整工具调用的坏响应可以有界重新请求模型，但坏 output 不进入可执行调用集合。
6. RuntimeResolver 最低支持 mainagent/planner/worker/verifier，允许不同模型但默认回退同一旧配置。快照只记录非敏感配置/别名与版本；Provider 不保存共享可变 messages 或 previous_response_id。不同 Session 的相同 call_id 不串线。
7. 提供跨角色的上游请求并发限制（与 Worker 池 N 分开），尊重服务端配置并能观察 queued/in_flight。所有角色的调用策略、预算来源和暂停/取消资格必须明确。
8. 保留 Responses NativePayload 的持久化与检查点不变量；不能通过丢掉所有原生 reasoning/history 让测试过关。输出长度配置可增加可解释限制，不能无限 token。

## 必须验证

两种协议的完整历史、多工具批次中断、坏 JSON/重复 call_id、截断/refusal、429/5xx/超时、取消排队/退避、最大重试数与总预算；至少 4 个并发 Session 交错响应且相同 call_id 仍隔离；N=4 模型请求上限2；恢复不会重复已成功外部调用。现有响应恢复和 compaction 测试保持。

交付与 1 的最小接口用例、脱敏协议 fixtures、非敏感配置示例、实测命令，以及没有真实模型 smoke 时的明确未验证声明。不把协议替身通过写成真实服务稳定。
