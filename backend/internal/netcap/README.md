# M2 网络来源与捕获网关

本包是 Worker 的直接 HTTP/HTTPS 传输，也是命令网关的唯一受信请求/响应来源。收据只由 `*Transport` / `*Gateway` 在真实交换之后写入。模型 JSON、命令 stdout 和 `MemGateway` 都不能变成 empirical 证据。

## ARTEX 对应与有意差异

参考只读源码：`ARTEX/traffic/traffic.go`、`ARTEX/db/finding_traffic.go`、`ARTEX/agent/finding_recorder_test.go`。

ARTEX 用嵌入的 go-mitmproxy 记录 Worker 流量，Finding 通过已登记的 `traffic_id` 绑定证据；未登记的 ID 不能升级为发现。本包保留“先记录交换，再引用收据”的分界。

有意不做的 ARTEX 行为：

- 不引入 go-mitmproxy，不设置 `SslInsecure: true`，不在 MITM 失败后 fail-open 透传。
- CONNECT 未解密时只记 `connect_metadata`，不能当正文证据。
- 不把本项目的 epoch、预算和授权账本说成 ARTEX 已有设计。
- 额外拒绝打到本部署的管理端口 `25432`、`28080`、`23000` 和元数据地址 `169.254.169.254`。明确授权的内网靶场仍然允许。

没有 `execution_input.network_policy` 的历史快照保持只读 HTTP GET。发现策略里的 POST/HTTPS 不会被静默升级。`get_endpoint` 仍是 HTTP GET、不跟随重定向、5 秒、64KiB、`User-Agent: AgentWorkbench-Trace/1.0`。

## 生产注入

生产必须显式安装同一个 `*Transport`，再把它交给 Gateway。没有安装时，直接工具可以自建真实 Transport，但不会共享进程内 singleflight。禁止把 `MemGateway` 或 `contract.Unimplemented*` 当作生产兜底。

```go
transport := netcap.NewTransport(netcap.NewStore(pool))
netcap.Install(transport)
if err := netcap.RejectNonProduction(transport); err != nil { /* 不要启动 */ }
gw := netcap.NewGateway(transport, mustAuthority())
if err := gw.Listen("127.0.0.1:28183"); err != nil { /* 预留端口，测试用 :0 */ }
```

`RejectNonProduction` 拒绝 nil 和带 `ProductionForbidden()` 的替身。真实 `*Transport` 没有该方法。

## Agent 4 网关接入

Agent 4 只依赖冻结的 `contract.CaptureGateway`，加上下面这些具体方法。不要挂载 Docker socket，不要让模型传入 `run_id` 当授权。

1. `Listen(addr)` 后用 `ProxyURL()` 作为容器的 `HTTP_PROXY` 和 `HTTPS_PROXY`。`NO_PROXY` 必须为空。
2. `CACertPEM()` 只含公钥。挂进执行镜像的 `SSL_CERT_FILE`。私钥留在网关进程。
3. `IssueJobCapability(ctx, scope, ttl)` 签发 bearer token。scope 必须通过 `Validate()`，并且带服务器分配的 `job_id`、`run_id`、`epoch`。Worker 还要有 `work_item_id` 和 `attempt_id`。库里只存 token 的 sha256。
4. 容器请求带 `Proxy-Authorization: Bearer <token>`。也接受 Basic 用户名 `bearer`。`X-Run-Id`、`X-Epoch`、`X-Job-Id` 会被丢掉。
5. 明文 HTTP 由 Gateway 转发并生成 `direct` 收据。HTTPS 走 CONNECT。网关持有 CA 时解密并生成 `tls_intercept` 收据；上游证书仍按系统根加该 origin 的 fixture CA 校验。
6. 未拦截的 CONNECT 只保存 `connect_metadata`。精确 path 授权下的不透明 CONNECT 会被拒绝。
7. `AllowContainerDial(addr)` 只允许拨到代理监听地址。其他地址，包括 fixture 自身，返回 scope denied。Agent 4 要在容器网络命名空间里调用它；本包只验证了函数和代理拒绝，没有启动 Agent 4 的容器。
8. `Lookup` 只返回同一 Run 和 Epoch 的 gateway 收据。`Revoke` 取消匹配的在途交换并撤销 capability。

直接 `http_request` 的 `http.Transport.Proxy` 固定为 nil，不读环境变量 `HTTP_PROXY`。命令链若需要 HTTPS 正文，必须走上面的受信 CA，不能全局关闭证书校验。

## 来源示例

冻结形状见 `backend/internal/contract/testdata/http_receipt.json`：

```json
{
  "request_id": "77777777-7777-4777-8777-777777777777",
  "receipt_id": "66666666-6666-4666-8666-666666666666",
  "asset_id": "88888888-8888-4888-8888-888888888888",
  "method": "POST",
  "url": "https://fixture.local/items",
  "status_code": 200,
  "response_preview": "{\"ok\":true}",
  "capture_mode": "tls_intercept",
  "credential_ref": "ref:session-alias"
}
```

实际行由 `SaveExchange` 写入 `http_receipts`，`minted_by` 只能是 `gateway`。它另外绑定 run、epoch、work item、attempt、session、job、`source_tool_call_id`、请求/响应哈希和 credential version。请求和响应正文在 `http_capture_artifacts`，不进旧的 `artifacts` 表，避免 `source_tool_call_id` 唯一约束冲突。

`http_request` 成功输出包含嵌套的 `receipt`。`Execute` 不收到 tool call id；Runner 应先插入 `status=pending` 的 `agent_tool_calls`，参数 JSON 与工具收到的参数一致。工具返回后调用 `Store.BindPendingToolCall` 把收据绑到该行。empirical 要求工具名为 `get_endpoint` 或 `http_request`，状态 success，收据 minted、绑定同一 call、同一 run/epoch/asset，`capture_mode` 为 `direct` 或 `tls_intercept`，outcome `completed`，未截断，哈希与 artifact 一致。

预览最多 512 个 rune，敏感 JSON 键和头会脱敏。完整正文用 `ReadArtifact`，必须匹配 run 和 epoch。敏感 artifact 在未授权读取时只返回预览。

认证只用 `credential_ref` / `session_ref` 的 `ref:` 别名。密钥在 `http_credentials`，按 target origin 绑定。会话默认属于创建它的 work item；跨方向共享必须走服务器 `PutSharedSession`。没有全局可变 cookie jar。跨 origin 重定向会去掉 Cookie、Authorization 和 X-Api-Key。

用户选择只读或主动模板写在 `execution_input.network_policy`。模型输入没有 allow 字段，多出来的 `allow` 会被忽略。

GET/HEAD 的进行中相同请求会 singleflight。完成后再次请求会重新打到网络。POST 不去重。页槽在发送前占住，取消于发送前会释放，因此不消耗 `max_pages`。网络发生在 pace 事务提交之后。

`replay_safe` 只给无凭据、无会话、无 body、已完成且未截断的 GET/HEAD。认证或有副作用的原始请求不能自动重放；请求把复测标成 `fixed` 会被拒绝，派生路径记 `inconclusive`，摘要包含原因。旧的无收据 GET 仍按保存的 vulnerable/fixed 条件复测。

## 交给 Agent 0 的接线

不要在本分支改这些文件：

- `backend/internal/tools/capability.go`：注册 `tools.HTTPRequestAdapter()`。
- Worker 工具装配：用已 `Install` 的 Transport 构造 `HTTPRequestTool`。不要改 Engine 来兜底串行。
- `service/run.go`：只有用户显式提交 `network_policy` 时才写入 `execution_input`。不要给旧快照补 HTTPS 或额外方法。HTTPS seed 仅在该 origin 已被显式授权时允许。
- `cmd/server/main.go`：构造 Transport 和 Gateway，`Install`，`Listen`，设置 `ExecutionPorts.Transport` 和 `Capture`，并调用 `RejectNonProduction`。不要把 Unimplemented 当 fallback。
- Compose / Agent 4 镜像：挂载 `CACertPEM()`，代理指向 `ProxyURL()`，带 Job capability。容器除代理地址外的拨号必须失败。
- `orchestration/retest.go`：自动重放前如果 `ReplayUnsafe`，直接结束为 inconclusive，不要调用工具。HTTPS 自动复测需要已注册的 `http_request`；`get_endpoint` 不能重放 HTTPS。
- `session_runner.go`：对 `http_request`，工具上下文超时至少要覆盖请求超时，上限 `contract.MaxHTTPToolTimeout`。预算默认 10 秒会截断 30 秒请求。

## 迁移

`backend/migrations/028_http_capture.up.sql` 新增凭据、会话、捕获 artifact、收据、origin pace、页槽、请求账本和 job capability。列名是 `is_binary`，因为 PostgreSQL 保留 `binary`。

向下迁移在存在 `http_receipts` 或 `http_credentials` 时拒绝执行。空库可以回滚。应用前确认 `schema_migrations` 没有 dirty 的 28。

## 本轮验证

数据库只使用 `postgres://postgres:postgres@127.0.0.1:25432/agent_workbench_ma3?sslmode=disable`。测试会 TRUNCATE。端口来自 `httptest` / `:0`。没有调用付费模型，没有访问外部靶场，没有重启开发服务。

```text
TEST_DATABASE_URL=.../agent_workbench_ma3 go test ./internal/netcap/ -count=1 -timeout 180s
ok  agent-workbench/backend/internal/netcap  2.979s

TEST_DATABASE_URL=.../agent_workbench_ma3 go test ./test/ -count=1 -timeout 180s -run 'TestMAHTTP|TestHTTPTraceability'
ok  agent-workbench/backend/test  15.279s
```

覆盖了本地 HTTP 和自签 HTTPS、两个登录身份不串 cookie、JSON/表单、跨 origin 重定向剥离认证、缺失凭据不拨号、错误证书不记 completed、取消和截断、N=4 页上限与 pacing、进行中去重后再次请求仍打网络、伪造收据、知识查询、命令 stdout、旧 epoch、认证请求不能标 fixed、旧 GET 复测先 still_reproducible 再 fixed、原始 artifact 可读。未认证代理返回 407。伪造 `X-Run-Id` 不能代理策略外 origin。

未验证：Agent 4 容器网络命名空间里的 direct egress、Compose 与 Engine 接线、浏览器、付费模型、外部靶场。任意 TCP、完整漏洞利用库和认证管理 UI 按任务暂缓。
