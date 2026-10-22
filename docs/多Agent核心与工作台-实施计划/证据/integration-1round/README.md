# 集成分支一轮验收抽查

日期：2026-10-22。分支 `codex/ma-integration`。这不是 A01–A16 门禁，也不是 20 轮 `-race` 结果。

命令：

```bash
cd /home/hjdssj/project/personal/oma-worktrees/ma-integration/backend
MA_ACCEPTANCE_ROUNDS=1 \
MA_ACCEPTANCE_EVIDENCE=/tmp/ma-integration-acceptance \
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:25432/agent_workbench_ma_integration?sslmode=disable' \
  go test ./test/ -count=1 -timeout 20m -run 'TestMA_A'
```

结果：54.534s，退出码 1。没有 `-race`。只跑 1 轮。数据库是 `agent_workbench_ma_integration`。没有调用付费模型，没有启动 Compose，没有连接开发库。

| ID | 一轮结果 | 第一条失败 |
| --- | --- | --- |
| A01 | 通过 | discovery N=4 与非发现 N=4 的 barrier、running attempt、server peak 都是 4 |
| A02 | 失败 | worker 被挡住时 follow-up planner 调用没有开始 |
| A03 | 失败 | A 仍被持有时 B arrived=0 |
| A04 | 通过 | 请求的 `worker_max_model_calls=3` 被写入；耗尽方向 4 次后 `worker_settlement_complete` |
| A05 | 失败 | 429 后没有 5xx 恢复 |
| A06 | 失败 | N=4 时模型峰值 in-flight=4，没有共享上限 2 |
| A07 | 失败 | `kill_work` 已注册，但替身没给 `work_item_id`，目标仍是 running |
| A08 | 失败 | 消息提交和幂等回放没有失败；人类 `POST /commands` 的 `grant_budget` 返回 200。该路由不是 Main Agent |
| A09 | 失败 | HTTPS 种子仍返回 `only http scheme is permitted for seed URL` |
| A10 | 失败 | catalog 峰值 in-flight=2 |
| A11 | 失败 | 测试启动路径没有挂上 `run_command`，没有 `execution_jobs` 行 |
| A12 | 失败 | stop 后 `in_flight=1` |
| A13 | 失败 | finish 仍要求结构化 `source_tool_call_id` |
| A14 | 失败 | workers 没有把材料分开 |
| A15 | 失败 | Playwright 没有对隔离栈运行 |
| A16 | 失败 | 026 迁移已存在，发布门禁用例仍按空号失败 |

`证据/ma7/` 仍是合入前基线，本目录不覆盖它。
