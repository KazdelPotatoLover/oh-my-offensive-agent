# oh-my-offensive-agent — Phase 1: Task Execution Foundation (Agent Workbench)

A monolithic application for managing tasks and their execution runs, with a Go backend, Next.js frontend, and PostgreSQL database. Phase 1 establishes the core task lifecycle — creating tasks, starting runs with a simulated worker, tracking progress through events, and handling stop/failure/restart scenarios — without any LLM or real tool integration.

## Architecture

The application consists of three components:

| Component | Technology | Role |
|-----------|-----------|------|
| **Backend** | Go (net/http) | REST API server + FakeWorker execution engine |
| **Frontend** | Next.js (React) | Dashboard UI with polling-based updates |
| **Database** | PostgreSQL 16 | Persistent storage for tasks, runs, and events |

### Backend Modules

| Module | Responsibility |
|--------|---------------|
| `handler/` | HTTP request routing, JSON serialization/deserialization, error responses |
| `service/` | Business logic, validation, transaction management, idempotency enforcement |
| `worker/` | FakeWorker execution engine — runs tasks as goroutines, emits progress events |
| `db/` | Database connection pool, schema migrations, startup recovery of interrupted runs |
| `model/` | Data structures and type definitions (Task, Run, RunEvent, enums) |

### Frontend Modules

| Module | Responsibility |
|--------|---------------|
| `lib/api.ts` | API client wrapping all backend endpoints, with 2-second polling for run status |
| `components/` | Reusable UI components (task list, run detail, event timeline, status badges) |

## Data Model

### Entities

- **Task** — A named unit of work (e.g., "Summarize document"). Tasks are containers for runs.
- **Run** — A single execution attempt of a task. Each run progresses through a state machine and records its history as events.
- **RunEvent** — An immutable log entry recording something that happened during a run (state change, progress step, error).

### Relationships

```
Task 1 ──── * Run 1 ──── * RunEvent
```

A task can have many runs (retries, re-executions), and each run can have many events.

### Key Constraints

- **At most one active run per task** — enforced via a partial unique index on `(task_id) WHERE status NOT IN ('completed', 'failed', 'stopped', 'interrupted')`. This prevents concurrent execution of the same task.
- **Idempotency key uniqueness per task** — each `(task_id, idempotency_key)` pair must be unique, enabling safe request retries without creating duplicate runs.

## State Transitions

Runs follow a strict state machine:

```
queued → running → completed
                 → failed (at configured step)
                 → stopping → stopped
        → interrupted (on server restart)
queued → interrupted (on server restart)
queued → stopped (if stop requested before worker picks up)
```

### Rules

- **Terminal states are final** — once a run reaches `completed`, `failed`, `stopped`, or `interrupted`, no further state changes or progress events are accepted.
- **Transactional consistency** — state changes and their corresponding events are always saved in the same database transaction.
- **FakeWorker behavior** — outputs 1 progress event per second for 10 steps (configurable via `total_steps`), then completes. If `fail_at_step` is set, the worker fails at that step instead of completing.

## Quick Start

### Option 1: Docker Compose (recommended)

```bash
docker compose up -d --build
```

- Frontend Dashboard: [http://localhost:23000](http://localhost:23000)
- Backend API: [http://localhost:28080](http://localhost:28080)
- PostgreSQL Port: `25432`

> **提示 (WSL2 / Windows 访问说明)**:
> - 浏览器可直接访问 [http://localhost:23000](http://localhost:23000) 或 [http://127.0.0.1:23000](http://127.0.0.1:23000)。
> - 项目采用容器 Host 网络与 Next.js 反向代理设计，完美兼容 WSL2 Mirrored 镜像网络模式。若开启了 Clash / Mihomo 等系统代理工具，建议访问 `http://localhost:23000` 或在代理规则中确认本地回环已绕过。


### Option 2: Local Development

```bash
# Start PostgreSQL
docker compose up -d db
```

```bash
# Backend (terminal 1)
cd backend
export DATABASE_URL='postgres://postgres:postgres@localhost:25432/agent_workbench?sslmode=disable'
export PORT=28080
export CORS_ORIGINS=http://localhost:23000
go run ./cmd/server
```

```bash
# Frontend (terminal 2)
cd frontend
export NEXT_PUBLIC_API_URL=http://localhost:28080
npm run dev -- -p 23000
```

Open [http://localhost:23000](http://localhost:23000) to access the dashboard.

## Verification Scenarios

Use these scenarios to verify correct behavior after deployment:

### 1. Normal Completion

Create a task → Start a run → Watch the event timeline as 10 progress steps appear (one per second) → Run status becomes `completed`.

### 2. Duplicate Start Blocked

While a run is actively executing, try starting another run on the same task → The API should return an error: **"task already has an active run"**.

### 3. Concurrent Start (Idempotency)

Start a run with header `Idempotency-Key: test-1` → Start another run with the same key and same parameters → The API returns the **same run** (same ID, same status) instead of creating a duplicate.

### 4. Idempotency Conflict

Start a run with `Idempotency-Key: test-2` and `fail_at_step: 3` → Start another run with the same key but `fail_at_step: 5` → The API returns **409 Conflict** because the parameters don't match the original request.

### 5. Specified Failure

Start a run with `fail_at_step: 5` → Watch 4 progress steps appear → Run fails at step 5 with reason **"failed at step 5 (simulated)"**.

### 6. Stop

Start a run → While it's running, click the **Stop** button → Run status transitions to `stopping`, then to `stopped` once the worker acknowledges the cancellation.

### 7. Server Restart Recovery

Start a run → Kill the backend process (Ctrl+C or `kill`) → Restart the backend → The previously active run's status should now be `interrupted` with reason **"server restarted"**.

### 8. History Preserved

After running multiple runs on a single task, open the task detail view → All historical runs are listed with their events, showing the full execution history.

### Automated Test Suite & Verification Results

All 8 scenarios are fully verified via automated integration tests against real PostgreSQL:

```bash
cd backend
go test -v -timeout 60s ./test/...
```

Test Results Summary:
- `TestScenario_NormalCompletion`: **PASS** (10 progress steps, 13 transactional events with seq 1..13, final status `completed`)
- `TestScenario_DuplicateStartBlocked`: **PASS** (Attempting to start a second run while one is active returns `409 Conflict` via DB partial unique index `idx_runs_one_active_per_task`)
- `TestScenario_ConcurrentStart`: **PASS** (5 concurrent requests with identical `Idempotency-Key` race safely and return the exact same run)
- `TestScenario_IdempotencyConflict`: **PASS** (Same `Idempotency-Key` with differing `fail_at_step` returns `409 Conflict`)
- `TestScenario_SpecifiedFailure`: **PASS** (`fail_at_step: 3` executes steps 1 and 2, fails at step 3 with reason `failed at step 3 (simulated)`)
- `TestScenario_StopRun`: **PASS** (Stop request transitions run to `stopping`, worker exits, final status transitions to `stopped` with reason `user requested stop`)
- `TestScenario_RestartInterruptedAndHistory`: **PASS** (Active runs transitioned to `interrupted` upon restart with event recorded; subsequent new runs execute normally and historical runs are preserved)
- `TestScenario_TerminalRunRejectsLateProgressAndReactivation`: **PASS** (Terminal runs reject late progress events and disallow transition back to active status)

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/tasks` | Create a new task |
| `GET` | `/api/tasks` | List all tasks |
| `GET` | `/api/tasks/:id` | Get task details |
| `PUT` | `/api/tasks/:id` | Update a task |
| `DELETE` | `/api/tasks/:id` | Delete a task |
| `POST` | `/api/tasks/:id/runs` | Start a new run for a task |
| `GET` | `/api/tasks/:id/runs` | List all runs for a task |
| `GET` | `/api/runs/:id` | Get run details with events |
| `POST` | `/api/runs/:id/stop` | Request a run to stop |

## Known Limitations (Phase 1)

- **No LLM integration** — FakeWorker only; no language model calls
- **No real tool execution** — worker simulates work with sleep + progress events
- **No multi-agent support** — single worker type, no agent-to-agent communication
- **No graph/workflow engine** — runs are flat sequences, no DAG-based orchestration
- **No MCP protocol support** — no Model Context Protocol integration
- **No traffic proxy** — direct communication only
- **No WebSocket/SSE** — frontend uses polling (2-second interval) for status updates
- **No authentication/authorization** — all endpoints are unauthenticated
- **No automatic retry of interrupted runs** — interrupted runs stay in terminal state
- **Single-node only** — no distributed locking or multi-instance coordination
