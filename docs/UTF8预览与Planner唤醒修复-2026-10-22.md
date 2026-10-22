# UTF-8 响应预览与 Planner 唤醒修复

日期：2026-10-22。本次修改代码并使用隔离数据库和本地 HTTP 服务验证，没有请求靶场或真实模型，没有修改任务参数、既有运行记录或部署服务。

## 根因与修复

`backend/internal/netcap/redact.go:redactBodyPreview` 在检查完整正文 UTF-8 合法性、脱敏后，直接截取前 512 字节。中文或 emoji 跨越边界时，预览变成非法 UTF-8，无法写入 PostgreSQL 的 `http_receipts.preview`，进而使已经收到 HTTP 200 的工具调用失败。

修复为在 512 字节内退回到完整字符边界。仍先脱敏再截断，预览仍不超过 512 字节；完整捕获正文和来源产物继续独立保存。原本非法编码或二进制正文仍返回 `[binary]`。

`backend/internal/orchestration/scheduler.go` 原来只在 `waitPlannerSignal` 收到 channel 通知时做 debounce。事实写入、Worker 结束和 Nudge 先置位 dirty/idle，`waitUntilPlannerNeeded` 可以直接返回，绕过通知合并；数据库轮询发现变化也走这条路径。修复为所有进入下一规划轮的路径统一先执行有界合并，期间响应取消。

对 `d48c05d9` 的只读核对显示：配置 `max_planner_rounds=8`，记录为初轮加 8 个后续评估轮，其中 6 个后续轮为 `event_wake`。最终 `status=stopped`、`finish_reason=needs_input`。现有计数口径限制后续评估轮，真实事件评估同样计数；本次保留该硬上限，不通过豁免事件轮次扩大授权。此次修复消除已复现的合并旁路，不声称所有历史轮次都是重复唤醒，也不保证运行一定用满时间预算。

## ARTEX 对照与有意差异

只读核实相邻 ARTEX 的 `server/engine.go:plannerLoop`：`runRound` 在事件和心跳路径统一执行 debounce，每次实际 `planner.Plan` 记录一轮；`agent/planner.go:todoFor` 跨轮共享待办。此次修复对齐的是服务端通知合并位置，没有改变 Planner 每轮新会话的机制。

本工程继续使用 PostgreSQL 变更流、轮前游标和显式轮数/模型/工具硬额度。这些属于本工程适配，不是 ARTEX 原样预算实现。规划期间产生的新 Worker 事实仍需下一轮审阅，不能在轮末把全部新变化一起标为已读。

## 实际验证

先新增测试复现旧实现失败，再修复。数据库测试使用临时 PostgreSQL 16 容器中的 `agent_workbench_ma_integration`，与开发库隔离；模型使用测试替身，HTTP 使用本机临时端口。

| 验证 | 结果 |
| --- | --- |
| 中文、双字节字符、emoji 在 512 字节边界的所有切割偏移；ASCII、精确边界、脱敏、二进制 | 通过；旧实现产生非法 UTF-8 |
| dirty 唤醒的通知合并、合并期间取消、新事件再次唤醒 | 通过；旧实现绕过等待 |
| `go test -race ./internal/netcap -count=1 -timeout 180s` | 通过，7.876s；包含中文预览实际入库、完整正文读取及既有 HTTP 捕获回归 |
| `go test -race ./internal/orchestration -count=1` | 通过，1.189s |
| `go test -race ./test -run 'TestMARuntime\|TestP3_\|TestOrchestration_DualWorker_ParallelExecution_Mock\|TestReview2_ConcurrentWorkers_KillTargetWorkerOnly\|TestDiscovery_FullChain_MockAndFixture' -count=1 -timeout 180s` | 通过，31.600s；包含 N Worker、在途事实唤醒、独立派工、局部失败、暂停及定向停止 |
| `go test -race ./test -run TestDiscovery_ChineseResponsePersistsAndExtractsLinks -count=1 -timeout 45s` | 通过，2.360s；实际 SessionRunner 执行 `get_endpoint → extract_endpoint_links`，两次工具成功，并登记预览边界之外的链接及来源 |
| `go vet ./internal/netcap ./internal/orchestration ./test` | 通过 |

没有迁移、前端改动或新增预算配置。未重新执行原真实任务，修复后的实际探索覆盖率需在更新运行服务后验证。
