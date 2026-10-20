# 第七阶段补充：Responses API 适配

## 1. 范围与当前状态

用户要求支持 Responses API。协议适配、会话持久化、配置入口和前端显示现已实现；本文保留设计与验收要求。具体测试结果见《第七阶段-Responses实现与验收》。本地协议验收不代表已接通真实服务。

扩展现有 Provider 边界，保留 Engine、Planner/Worker 职责、领域工具、任务状态与证据处理。消息 DTO、会话执行器和消息持久化需要小范围扩展，不能只替换请求 URL。

第一版支持非流式、客户端管理会话历史、函数工具调用、最终文本、拒绝、错误与用量。先不加入服务端 Conversations、previous_response_id、后台响应、流式输出、原生浏览器或其他托管工具。

## 2. 配置与 Provider 工厂

新增显式配置：

```dotenv
LLM_API_FORMAT=responses
LLM_BASE_URL=https://your-gateway.example/v1
LLM_API_KEY=由用户在本地填写
LLM_MODEL=服务商实际支持的模型ID
```

LLM_API_FORMAT 支持 `chat_completions` 和 `responses`，为兼容旧配置默认 chat_completions。Base URL 为协议根地址，程序分别追加 `/chat/completions` 或 `/responses`。不要把完整请求路径误当根地址；不要尝试一个协议失败后自动改用另一个协议或 Mock。

Compose 后端映射上述配置，前端不接收密钥。Run 快照保存实际格式、模型、脱敏端点和非敏感请求设置。执行过程中使用本 Run 生效的设置，协议不能在 Session 中途改变。

将两个 handler 分支里重复的 Provider 创建整理成一个工厂。Provider 标识表示真实服务或 Mock，API format 表示网络协议，避免混成同一个枚举。未知格式明确拒绝。

## 3. 协议转换的核心

Responses 使用类型化 input/output Items，而非 Chat Completions 的 messages/choices 结构。[官方迁移说明](https://developers.openai.com/api/docs/guides/migrate-to-responses)

| 内容 | Responses 处理 |
| --- | --- |
| 用户或系统输入 | 转成相应 message Item，保持角色与先后顺序 |
| 函数工具定义 | type、name、description、parameters 在函数定义层级，不套用 Chat 的 function 包装 |
| 工具请求 | 解析 function_call Item 的 name、arguments 和 call_id |
| 工具结果 | 用 function_call_output Item 回交，call_id 对应原调用 |
| 可见回答 | 从 message 的 output_text 内容提取，不能假设 REST JSON 有 SDK 的 output_text 辅助字段 |

将 function_call.call_id 映射为内部 ToolCall.ID；Item 的 id 是另一种标识，不能用它代替 call_id。遍历整个 output，支持同轮多个函数调用与文本并存；存在工具调用时先处理所有调用，不能因为也有文本就结束会话。

现有 Schema 有可选字段。初版可显式发送 strict:false，继续依赖服务端程序校验；若改成 strict:true，必须完整满足对应 Schema 约束，不能只切一个开关。[官方函数工具说明](https://developers.openai.com/api/docs/guides/function-calling)

## 4. 保留原生输出，保证下一轮可继续

当前 ModelResponse 只保留文本和工具调用，这会丢掉 Responses 的其他会话信息。对于推理模型，后续输入需要保留返回的 reasoning Items 及相关工具 Items；手动管理历史时应保存可回传的原生输出，不能只重建一条文本 assistant 消息。[官方推理上下文说明](https://developers.openai.com/api/docs/guides/reasoning)

建议在内部响应与 assistant 消息上增加协议标记和原生输出载荷，具体命名由实现 Agent 按代码风格决定。至少实现：

1. Provider 解析可见文本、函数请求，同时返回本轮可回传的 output Items。
2. SessionRunner 与旧 AgentRunner 把该载荷附在当前 assistant 消息，写入会话记录，再执行工具。
3. 下一轮 Responses 请求中，该 assistant 消息展开为保存的原生 Items；不要又追加一份重建的文本与函数调用，造成重复。
4. 工具结果按原 call_id 排在对应原生输出之后。
5. 为 agent_messages 增加可空载荷存储，旧历史保持可读；消息从数据库重新加载后仍可重建同样的下一轮请求。不需要在本阶段自动恢复中断 Run。
6. 所有载荷限定在当前 Session，不保存在共享 Provider 实例的可变全局历史中。

初版请求显式 store:false、stream:false，客户端回传完整的必要历史。按实际端点能力处理返回的 encrypted_content，作为不透明状态保存与回传，不解码、不渲染成思考文本。官方当前文档说明 stateless 模式会返回该状态，兼容旧端点时可显式配置 include，但不能默默丢弃已返回状态。

保留可重放的 Item 字段，例如 message phase、reasoning encrypted_content、函数 Item 的原始标识和顺序。按照官方输入类型处理需要 output→input 转换的字段，并用 stub 验证。无需把这些内部状态默认暴露给浏览器或普通日志。

## 5. 状态与用量

Responses 的响应状态不能直接当作“工具循环已结束”。响应 completed 且包含 function_call 时，内部应继续进入工具执行；completed 且有有效最终文本、没有待处理工具请求时，才可结束当前会话。

- incomplete 根据原因映射为截断或明确错误，不能误报成功。
- failed、cancelled、意外的 queued/in_progress 与非 2xx，返回可区分错误。
- refusal 内容映射到现有拒绝路径。
- 只有 reasoning、没有工具调用或有效最终答案时，按明确规则有限继续或报协议错误，不能完成 Run。
- 如返回明确的非最终 message phase，不能把它的文本直接作为最终答案。
- 缺失或重复 call_id 在执行这一批工具前拒绝；不支持的可执行 Item 明确报错。

将 usage.input_tokens、output_tokens、total_tokens 映射到现有用量结构；缺失用量保持未知。响应体 response.id 与 HTTP x-request-id 分别保存，避免混淆。现有 RequestID 字段如需扩展，用新增字段或元数据保存响应 ID。

请求继承当前 Session 的超时与取消上下文，受 Run 总预算约束。保持不自动重试工具请求的策略；模型返回工具调用并不替代程序的参数与权限检查。

## 6. 必要测试

先使用本地 HTTP stub，不需要真实密钥或对外调用，验证：

- /responses 路径、Authorization、非流式请求、store:false、正确工具定义。
- 第一轮包含 reasoning、函数调用和文本，第二轮保留原生 Items 并带正确 function_call_output。
- 多函数调用、缺失和重复 call_id、Item id 与 call_id 不同。
- 原生输出写库后重新加载，仍能构造同样的后续请求，Session 之间不混淆。
- incomplete、refusal、failed、空输出、非最终文本与未知可执行 Item。
- 请求被取消、超时、401/429/5xx 和用量映射。
- chat_completions 与 Mock 的已有回归继续通过，协议配置错误不静默回退。

再按第七阶段主文档，先真实模型固定样本工具循环，再小范围靶场编排。第三方服务声称兼容不等于已通过工具调用与多轮状态验证，报告分别列出 stub 与真实服务结果。

## 7. 交给实现 Agent

在当前 Provider 抽象上新增 ResponsesProvider 与显式 LLM_API_FORMAT 配置。扩展内部消息载荷和持久化，正确实现 call_id 关联、原生 Items 回传、状态与用量解析；保留现有 Chat Completions 和 Mock 路径。

复用现有 Engine、领域工具和证据存储，不新建一套 Agent 框架。完成 Compose/示例配置、前端协议显示、迁移及上述 stub/数据库测试。真实凭据由用户在本地配置；没有真实调用结果时注明未验证，不声称 Responses 已接通。

本补充与第七阶段作为一个任务交付，不另起新阶段。先以一种用户选定的服务完成实际闭环，再扩展其他模型。
