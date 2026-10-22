# Agent 4：受控命令、脚本、工作空间与进程生命周期

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

你负责 M2 的执行能力。分支 `codex/ma-executor`。Worker 必须能在真正隔离环境里运行脚本和工具，不能在 API/宿主机执行模型命令，也不能只提交一个 echo mock。

## 先读与所有权

读 ArtifactService/workspaces、capability、SessionRunner 的取消/工具预算合同、Manager 生命周期。参考 ARTEX worker.go 的 WorkingDir、BashEnv、ToolOutputDir、settlement、capture。

拥有新增 execution broker/runner/Job、命令及文件工具、执行镜像/部署片段、必要029迁移和 ma_execution_* 测试。root Compose、Engine/Manager 资源撤销接线、SessionRunner 逐工具超时策略由 0 集成；不得自己改公共生命周期。捕获网关和可信 HTTP receipt 归 3，先按冻结接口联调。

## 实施顺序

1. 提供 JobExecutor.Run/Cancel/Inspect 的真实实现和测试替身；固定命令协议、镜像白名单、作用域、workspace、超时/输出/退出码。先跑无网络 Python fixture，再联调 3 的受控网络。
2. 新增 run_command、write_file、read_file、inspect_command 能力工厂。命令只在允许镜像的专属执行容器中运行；模型不能指定 docker 参数、host PID/network、挂载路径或任意镜像。
3. API/执行容器不挂 Docker socket，不 privileged、不 host network、非 root、drop capabilities、受限 CPU/memory/pids/时长、只读根和受控可写目录。专用受信 broker 可以通过固定协议创建/停止容器，但绝不 eval/exec 宿主机命令。
4. 每个 Attempt 的执行环境不能读取其它方向文件或宿主 .env。工作空间按稳定 WorkItem 保存；新 Attempt 可从受控产物恢复文件，旧进程不复活。符号链接和目录穿越须真实测试。
5. 同步命令有明确最大时长，受 Attempt/Run deadline 夹逼；不要让现有全工具5秒上限使所有脚本误超时，也不能放宽全局 deadline。需要逐工具策略时按 0 合同接线。
6. Stop/Kill/工具超时/旧Epoch撤销杀进程组、后台子进程和容器；取消请求使用独立有界清理上下文。Broker/API 重启后 reconcile 未终态 Job，不能进程继续跑而数据库已当完成。
7. 保存脚本、stdout、stderr、退出码及完整有界产物，来源关联原工具/Job/Attempt。保留 Artifact 现有 source 唯一语义，用 Job-artifact 关系承载子产物，不为了方便删原约束。exit_code!=0 是可分析的执行结果，不天然是整个 Run failed。
8. 联调 3 的网关：容器 HTTP/HTTPS 通过可强制执行的出口隔离和捕获，禁止 direct egress/代理绕过。只有 HTTP_PROXY 环境变量不算完成。curl/Python 的有效网络响应可追到受信 receipt；普通 stdout/exit_code 不能冒充 empirical 来源。
9. 能力显式由 profile 启用；settlement 禁止启动新命令/探测，只能保存已有材料。未知结果用 inspect 查询，不能自动重复命令。

## 必须验收

真实容器运行 Python 分析脚本和受控 curl/HTTPS 请求；跨目录/宿主/越界网络均被拒绝；无限输出、后台子进程、timeout、单WorkerKill、RunStop、旧Epoch和API/Broker重启无泄漏；产物可恢复；伪造 stdout 不产生已核验漏洞。测试清理只处理本轮拥有的容器/网络/目录，不使用 docker prune 或删除他人资源。

原始 TCP/raw socket/Kali全家桶暂缓；本轮核心是可实际执行、可撤销、有范围和证据的 Web/脚本能力，不是把容器/输出接口建出来便宣布完成。
