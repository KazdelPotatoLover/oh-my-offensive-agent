# Agent 7：独立验收、并发反例与发布证据

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

你负责从 M0 开始写出可复现失败用例，并在最终集成分支验收 A01–A16。分支 `codex/ma-acceptance`。你不能通过改业务语义或放松断言把结果做绿；发现问题交给具体owner修复。

## 所有权

新增 backend/test/ma_acceptance_*、受控应用/模型 fixtures、独立前端 e2e（与6约定文件）、本目录验收证据。保留原测试，必要合同调整写理由并交0。Playwright共享配置/路由/依赖接线由0与6负责，不能同时编辑。

## 先建立基线

读本轮总计划、合同、只读故障基线；读现有并发、Responses、Pause/Kill/Epoch、证据/复测测试。独立空库跑旧回归，记录真实结果。检查实际快照/effective/runtime，不能先假定“用户的双Worker就是DB死锁”。

## 构造两个真实 fixture

1. 模型替身：Chat/Responses双格式，能用屏障控制4个Session交错与同call_id；可产生不完整function JSON/status、429/5xx/超时/refusal/截断；记录请求并发、重试及取消，不转发外网。
2. 受控业务应用：HTTP+TLS、登录与至少两个身份、不同材料和可重复的无害测试问题。一个方向发现入口，一个方向分析授权材料/生成脚本，另一个验证特征；修补后能独立验证。路径/marker可变，不能固定fixture名称就走一条硬编码生产流水线。

Fixture需要记录真正的目标请求和执行容器痕迹，不能先插一条success DB记录当网络完成。业务问题使用本地合成数据，不需要攻击外部目标。

## 核心反例与门槛

逐条实现 A01–A16：配置N=1/2/4包括discovery；4个Attempt真重叠、不超N、不重复；Planner模型调用期间Dispatcher可领取；A在途事实唤醒B；局部失败/耗尽而其它Worker继续；Provider共享限流和有限恢复；Planner主动steer/kill；Main真实动作/幂等/授权边界；HTTPS认证/身份隔离；并发origin限额；容器隔离/可信捕获；Stop/Kill/重启/Epoch无泄漏；证据/旧审核保留；完整浏览器。

用可控barrier/channel编排先后关系，记录峰值active与步骤起止，禁止用“最后完成4项”代替并发证明。关键A01–A08重复至少20轮，跑-race；必须有资源泄漏断言，包括running DB子记录、排队请求、容器及后台子进程。

Main的工具动作必须有真实command/session/WorkItem引用；运行卡片必须来自实时状态；坏响应绝不执行工具；重复请求不能触发额外副作用或新增授权。

## 浏览器真实操作

从UI创建授权任务→输入N=4→观察真实并发→Main聊天查询/纠偏→查看某方向trace/产物→Planner主动处理跑偏方向→单方向Kill其它继续→整体暂停/停止→授权追加与同RunEpoch续跑→看目标证据和最终交付→刷新恢复导航。另验证失败/断线/409/旧任务与复测。截图/trace只保存当次真实执行的结果。

## 最终验证和交付

- 每个测试用自己的临时DB，不指向agent_workbench，不与1/2/3/4共享会被清表的库。容器/网络/端口按测试唯一标识拥有，清理只删自己的，不执行prune。
- 最终集成commit下跑完整go test -race、go vet、frontend build、Playwright和Compose启动/smoke；旧库升级比对原证据/审核历史，不伪造过去的记录。
- 输出A01–A16表：状态、commit、命令、断言、原始证据路径；另列缺失与有意差异。测试失败就写失败，不预填PASS。
- 本地模型替身通过不能写真实商业模型稳定。提供需要用户显式给模型/目标/额度才启用的真实smoke入口，未运行注明未验证。
- 把根因、失败日志和最小复现交给owner，不未经协调改Engine或降并发；修复合入后复验。最终结果由0汇总，核心失败未解不得发布为完成。
