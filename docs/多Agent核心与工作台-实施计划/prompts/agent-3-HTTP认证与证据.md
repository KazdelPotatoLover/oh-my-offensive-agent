# Agent 3：真实 HTTP/HTTPS、认证会话与捕获证据

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

你负责 M2 的网络能力。分支 `codex/ma-http`。目标是让 Worker 超越只读 HTTP GET，并为直接工具和命令网关提供同一可信请求/响应来源。

## 先读与所有权

读 tools/http.go、service/run.go 的 seed/scope、discovery、verification/retest/artifact、capability 注册。本项目旧快照含只支持 http 的约束，不能静默把历史任务改为任意 HTTPS/方法授权。参考 ARTEX Worker 的 proxy/CA、流量检索和 Finding 证据绑定。

拥有新增 HTTP transport、http_request 工具、认证引用/会话、捕获 gateway 与受信 receipt、旧 get_endpoint 的 transport 适配、verification 的新来源解析、必要 028 迁移和 ma_http_* 测试。service/run.go 的通用 seed/策略接线、全局工具注册与 Compose 交 0，不直接改 Engine、SessionRunner 或命令执行实现。

## 实现步骤

1. 按冻结合同给出 TargetTransport/CaptureGateway 实现与本地替身，先让 4 能接入再扩展界面。生产与测试注入明确，不能生产 fallback mock。
2. http_request 支持 HTTP/HTTPS、GET/HEAD/POST 及授权的方法，query、普通头、JSON/表单/text body，timeout。服务器校验 Run/WorkItem/Attempt/Epoch、origin/path/方法；用户可显式选择只读/主动模板，模型不能给自己授权。
3. 认证用 credential_ref/session_ref；密钥来自服务器受控配置，按目标绑定。支持 fixture 登录/已有 Cookie 或 Token；默认会话隔离，跨方向共享必须显式引用，不能全局共享可变 cookie jar。模型、UI、普通日志看不到秘密值。
4. TLS 默认验证，受控 fixture 用专用 CA；重定向每跳验证 scope，禁止跨 origin 透传认证；连接/DNS 范围一致。允许明确授权的内网靶场，不用全面拒绝私网充当方案。
5. 并发下 origin pacing、页面/请求限额、去重与结果登记正确。锁内不执行网络。取消/超时/断流保存实际结果或 outcome_unknown；体积限制、二进制/截断明确。
6. 新 trusted receipt 绑定真正请求/响应、scope、调用或 Job、哈希和认证版本。response preview 与完整 artifact 分开，敏感字段脱敏和授权访问有界。
7. HTTPS CONNECT 透传不等于捕获正文；若命令链需要 HTTPS 正文证据，必须实现受信 CA 的解密捕获并让执行镜像信任该 CA，不能全局关闭证书校验。捕获网关为 4 的隔离命令提供 HTTP/CONNECT/CA 接口，只接受服务器签发且绑定 Job/Run/Epoch 的能力。不能模型编个 run_id 就代理任意目标。与 4 共同测试 direct egress 和代理旁路被拒绝。
8. report_finding 只认旧 get_endpoint 或新受信 HTTP 来源；知识查询、命令 stdout、伪造 receipt、旧 Epoch、别的资产/Session 都不能升级成 empirical。保留旧 GET 条件复测；无法安全自动重放的认证/副作用请求明确 inconclusive，不盲目重放。

## 必须验收

受控 HTTP+TLS fixture、登录与两个身份防串号、JSON/表单请求、跨 origin 重定向、伪造 receipt、证书错误、认证缺失、N=4 pacing/max_pages/去重、取消和超限、原始产物可查、旧报告与复测回归。使用真正本地 HTTP/HTTPS，而非构造一条假的 success 数据库记录代替网络链路。

交付完整网络来源示例、4 的网关接入说明和测试证据。任意 TCP/原始套接字、完整漏洞利用库和复杂认证管理 UI 暂缓；核心网络和来源闭环不能暂缓。
