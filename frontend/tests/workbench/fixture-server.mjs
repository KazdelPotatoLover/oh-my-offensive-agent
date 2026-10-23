// Explicit contract stand-in for the workbench browser tests.
// The Next app never selects this server. Playwright starts it and points INTERNAL_API_URL here.
import http from 'node:http';

const PORT = Number(process.env.WORKBENCH_FIXTURE_PORT || 23196);
const CREATION_BUDGET = { max_parallel_workers: 3, run_timeout_seconds: 0, max_model_calls: 0, max_tool_calls: 0 };
const CREATION_LIMITS = { schema_version: 'agent_limits.v1', task_timeout_limited: false, task_timeout_seconds: 0, model_calls_limited: false, tool_calls_limited: false, max_parallel_workers: 3, worker_run_timeout_seconds: 1200 };
const TASK = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const RUN = '11111111-1111-4111-8111-111111111111';
const LEGACY = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const TERMINAL = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const WORK = [
  '22222222-2222-4222-8222-222222222221',
  '22222222-2222-4222-8222-222222222222',
  '22222222-2222-4222-8222-222222222223',
  '22222222-2222-4222-8222-222222222224',
];
const ATTEMPTS = [
  '33333333-3333-4333-8333-333333333331',
  '33333333-3333-4333-8333-333333333332',
];
const SESSION = '44444444-4444-4444-8444-444444444441';
const FINDING = '55555555-5555-4555-8555-555555555551';
const FACT = '66666666-6666-4666-8666-666666666661';
const GOAL = '77777777-7777-4777-8777-777777777771';
const ARTIFACT = '88888888-8888-4888-8888-888888888881';
const ARTIFACT_OTHER = '88888888-8888-4888-8888-888888888882';
const PRIOR_COMMAND = '99999999-9999-4999-8999-999999999991';

function envelope(code, message, extra = {}) {
  return { error: { code, scope: extra.scope || 'request', message, retryable: extra.retryable === true, source_id: extra.source_id, suggested_action: extra.suggested_action } };
}

function taskRecord() {
  return {
    id: TASK,
    name: '授权入口探索',
    objective: '读取授权入口并保存响应',
    scope: { mode: 'discovery', seeds: ['http://127.0.0.1:29090/'] },
    config: {
      agents: {
        mainagent: { runtime_profile: 'default', model: 'configured-model' },
        planner: { runtime_profile: 'default', model: 'configured-model' },
        worker: { runtime_profile: 'default', model: 'configured-model' },
        verifier: { runtime_profile: 'default', model: 'configured-model' },
      },
      budget: {
        max_model_calls: 80,
        max_parallel_workers: 4,
        planner_reserved_model_calls: 16,
        main_reserved_model_calls: 8,
      },
    },
    created_at: '2026-10-22T00:00:00Z',
    updated_at: '2026-10-22T00:00:00Z',
  };
}

function runRecord(id, status, epoch) {
  return {
    id,
    task_id: TASK,
    status,
    current_epoch: 1,
    config_snapshot: {},
    execution_input: {
      executor: 'orchestrator',
      budget: { max_model_calls: 80, max_tool_calls: 200, max_parallel_workers: 4 },
    },
    created_at: '2026-10-22T00:00:00Z',
    updated_at: '2026-10-22T00:00:00Z',
    epoch_number_note: epoch,
  };
}

function fresh() {
  return {
    tasks: { [TASK]: taskRecord() },
    runs: {
      [RUN]: runRecord(RUN, 'running', 2),
      [LEGACY]: runRecord(LEGACY, 'completed', 1),
      [TERMINAL]: runRecord(TERMINAL, 'stopped', 2),
    },
    live: {
      status: 'running',
      epoch: 2,
      snapshotAt: '2026-10-22T00:00:00Z',
      budget: { max_model_calls: 80, max_tool_calls: 200, run_timeout_seconds: 600 },
      directions: [
        { work_item_id: WORK[0], objective: '读取授权入口并保存响应', source_refs: [FACT], status: 'running', attempt_id: ATTEMPTS[0], session_id: SESSION, latest_step: 'http_request', progress_at: '2026-10-22T00:00:01Z', local_budget: { model_calls: 2, tool_calls: 1, seconds: 30 }, allowed_actions: ['steer_work', 'kill_work'] },
        { work_item_id: WORK[1], objective: '保存入口页面快照', source_refs: [FACT], status: 'running', attempt_id: ATTEMPTS[1], session_id: SESSION, latest_step: 'browser_snapshot', progress_at: '2026-10-22T00:00:02Z', local_budget: { model_calls: 1, tool_calls: 1, seconds: 20 }, allowed_actions: ['steer_work', 'kill_work'] },
        { work_item_id: WORK[2], objective: '排队等待同源间隔', status: 'queued', allowed_actions: ['kill_work'] },
        { work_item_id: WORK[3], objective: '重试失败的响应比对', status: 'failed', latest_step: 'http_request', error: { code: 'tool_failed', scope: 'work_item', message: '响应比对失败', retryable: true, suggested_action: 'retry_work' }, allowed_actions: ['retry_work', 'continue_work'] },
      ],
      messages: [
        { seq: 1, id: '12121212-1212-4121-8121-121212121211', role: 'user', text: '查看授权入口', turn_id: '13131313-1313-4131-8131-131313131311', turn_status: 'completed' },
        { seq: 2, id: '12121212-1212-4121-8121-121212121212', role: 'assistant', text: '入口已记录，等待方向写回。', turn_id: '13131313-1313-4131-8131-131313131311', turn_status: 'completed' },
        { seq: 3, id: '12121212-1212-4121-8121-121212121213', role: 'action', text: '已引用既有纠偏命令', turn_status: 'completed', command_id: PRIOR_COMMAND, work_item_id: WORK[0] },
      ],
      nextSeq: 4,
      findingVerdict: '',
      events: [{ seq: 1, type: 'direction_progress', payload: { work_item_id: WORK[0], step: 'http_request' } }],
    },
    commandKeys: new Map(),
    messageKeys: new Map(),
    seq: 10,
  };
}

let state = fresh();

function bump() {
  state.live.snapshotAt = new Date().toISOString();
}

function snapshotFor(runId) {
  if (runId === LEGACY) return null;
  if (runId === TERMINAL) {
    return {
      schema_version: 'workbench.v1',
      snapshot_at: '2026-10-22T01:00:00Z',
      run: { id: TERMINAL, status: 'stopped', epoch_number: 2 },
      worker_capacity: { requested: 4, effective: 4, active: 0, queued: 0 },
      model_capacity: { limit: 2, in_flight: 0, queued: 0 },
      budget: { max_model_calls: 80 },
      goals: [],
      planner: { status: 'stopped', reason: 'Run 已停止' },
      directions: [{ work_item_id: WORK[0], objective: '读取授权入口并保存响应', status: 'completed', latest_step: 'http_request', allowed_actions: [] }],
      main_conversation: { last_seq: 0, active_turn_id: null },
      events: [],
      last_event_seq: 0,
      has_more_events: false,
    };
  }
  if (runId !== RUN) {
    const run = state.runs[runId];
    if (!run?.created) return undefined;
    const requested = Number(run.execution_input?.budget?.max_parallel_workers || 1);
    return {
      schema_version: 'workbench.v1',
      snapshot_at: run.created_at,
      run: { id: runId, status: 'queued', epoch_number: 1 },
      worker_capacity: { requested, effective: requested, active: 0, queued: 0 },
      model_capacity: { limit: 2, in_flight: 0, queued: 0 },
      budget: run.execution_input.budget,
      goals: [],
      planner: { status: 'idle' },
      directions: [],
      main_conversation: { last_seq: 0, active_turn_id: null },
      events: [],
      last_event_seq: 0,
      has_more_events: false,
    };
  }
  const active = state.live.directions.filter((item) => item.status === 'running').length;
  const queued = state.live.directions.filter((item) => item.status === 'queued').length;
  return {
    schema_version: 'workbench.v1',
    snapshot_at: state.live.snapshotAt,
    run: { id: RUN, status: state.live.status, epoch_number: state.live.epoch },
    worker_capacity: { requested: 4, effective: 4, active, queued },
    model_capacity: { limit: 2, in_flight: 2, queued: 1 },
    origin_pacing: { waiting: true, reason: '同源最小间隔未到' },
    budget: state.live.budget,
    goals: [],
    planner: { status: state.live.status === 'stopped' ? 'stopped' : 'waiting', reason: '等待当前方向写回', session_id: SESSION },
    directions: state.live.directions,
    main_conversation: { last_seq: state.live.messages.at(-1)?.seq || 0, active_turn_id: null },
    events: state.live.events,
    last_event_seq: state.live.events.at(-1)?.seq || 0,
    has_more_events: false,
  };
}

function modelCalls() {
  return Array.from({ length: 6 }, (_, index) => ({ id: `aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa${index}`, status: 'completed', round: index + 1 }));
}

function findings() {
  return [{
    id: FINDING,
    run_id: RUN,
    asset_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa10',
    check_result_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa11',
    title: '候选响应差异',
    category: 'behavior',
    severity: 'info',
    status: 'confirmed',
    description: '人工已确认的候选，不是事实本身',
    remediation: '',
    source_kind: 'empirical',
    verification_state: state.live.findingVerdict === 'fixed' ? 'retested' : 'verified',
    retest_verdict: state.live.findingVerdict,
    created_at: '2026-10-22T00:00:00Z',
    review_version: 1,
  }];
}

function send(res, status, body) {
  res.writeHead(status, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
  res.end(JSON.stringify(body));
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (chunk) => chunks.push(chunk));
    req.on('end', () => {
      const text = Buffer.concat(chunks).toString('utf8');
      if (!text) return resolve({});
      try { resolve(JSON.parse(text)); } catch (err) { reject(err); }
    });
    req.on('error', reject);
  });
}

function remember(map, key, hash, value) {
  const prior = map.get(key);
  if (!prior) {
    map.set(key, { hash, value });
    return { replay: false, value };
  }
  if (prior.hash !== hash) return { conflict: true };
  return { replay: true, value: prior.value };
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url || '/', 'http://127.0.0.1');
  const path = url.pathname;
  try {
    if (req.method === 'GET' && path === '/health') return send(res, 200, { ok: true });
    if (req.method === 'POST' && path === '/__fixture/reset') {
      state = fresh();
      return send(res, 200, { ok: true });
    }
    if (req.method === 'POST' && path === '/__fixture/retest') {
      state.live.findingVerdict = 'fixed';
      bump();
      return send(res, 200, { retest_verdict: 'fixed', snapshot_at: state.live.snapshotAt });
    }
    if (req.method === 'GET' && path === '/api/runtime/defaults') return send(res, 200, { budget: CREATION_BUDGET, limits: CREATION_LIMITS, model_configured: true });
    if (req.method === 'GET' && path === '/api/tasks') {
      return send(res, 200, { tasks: Object.values(state.tasks) });
    }
    if (req.method === 'POST' && path === '/api/tasks') {
      const body = await readBody(req);
      const id = crypto.randomUUID();
      const task = { id, name: body.name, objective: body.objective, scope: body.scope || {}, config: body.config || {}, created_at: new Date().toISOString(), updated_at: new Date().toISOString() };
      state.tasks[id] = task;
      return send(res, 201, task);
    }
    const taskRun = path.match(/^\/api\/tasks\/([^/]+)\/runs$/);
    if (taskRun && req.method === 'GET') {
      const runs = Object.values(state.runs).filter((run) => run.task_id === taskRun[1]);
      return send(res, 200, { runs });
    }
    if (taskRun && req.method === 'POST') {
      const body = await readBody(req);
      const id = crypto.randomUUID();
      const budget = { ...CREATION_BUDGET, ...state.tasks[taskRun[1]]?.config?.budget, ...(body.budget || {}) };
      delete budget.planner_reserved_model_calls;
      delete budget.main_reserved_model_calls;
      delete budget.name;
      const run = {
        id,
        task_id: taskRun[1],
        status: 'queued',
        current_epoch: 1,
        config_snapshot: {},
        execution_input: { executor: body.executor, provider: body.provider, budget },
        created_at: new Date().toISOString(),
        updated_at: new Date().toISOString(),
        created: true,
      };
      state.runs[id] = run;
      return send(res, 201, run);
    }
    const taskItem = path.match(/^\/api\/tasks\/([^/]+)$/);
    if (taskItem && req.method === 'GET') {
      const task = state.tasks[taskItem[1]];
      if (!task) return send(res, 404, { error: 'task not found' });
      return send(res, 200, task);
    }
    const runItem = path.match(/^\/api\/runs\/([^/]+)$/);
    if (runItem && req.method === 'GET') {
      const run = state.runs[runItem[1]];
      if (!run) return send(res, 404, { error: 'run not found' });
      return send(res, 200, run);
    }
    const events = path.match(/^\/api\/runs\/([^/]+)\/events$/);
    if (events && req.method === 'GET') return send(res, 200, { events: [] });
    const workbench = path.match(/^\/api\/runs\/([^/]+)\/workbench$/);
    if (workbench && req.method === 'GET') {
      if (workbench[1] === LEGACY) {
        return send(res, 501, envelope('not_implemented', 'workbench.v1 尚未在这条旧记录上提供', { scope: 'run', source_id: LEGACY, suggested_action: '使用旧运行页查看历史记录' }));
      }
      const snap = snapshotFor(workbench[1]);
      if (!snap) return send(res, 404, envelope('not_found', 'run not found', { scope: 'run' }));
      return send(res, 200, snap);
    }
    const messages = path.match(/^\/api\/runs\/([^/]+)\/main\/messages$/);
    if (messages && req.method === 'GET') {
      if (messages[1] !== RUN) return send(res, 200, { messages: [], last_seq: 0, has_more: false });
      const after = Number(url.searchParams.get('after_seq') || 0);
      const page = state.live.messages.filter((item) => item.seq > after);
      return send(res, 200, { messages: page, last_seq: state.live.messages.at(-1)?.seq || 0, has_more: false });
    }
    if (messages && req.method === 'POST') {
      await new Promise((resolve) => setTimeout(resolve, 700));
      const key = req.headers['idempotency-key'];
      if (!key) return send(res, 400, envelope('invalid_input', 'Idempotency-Key is required'));
      const body = await readBody(req);
      if (body.expected_epoch !== state.live.epoch) {
        return send(res, 409, envelope('epoch_conflict', 'expected_epoch 与快照不一致', { suggested_action: '使用快照 Epoch 重新提交' }));
      }
      const hash = JSON.stringify(body);
      const stored = remember(state.messageKeys, String(key), hash, null);
      if (stored.conflict) return send(res, 409, envelope('idempotency_conflict', 'Idempotency-Key 已用于不同内容', { suggested_action: '使用新的 Idempotency-Key' }));
      if (stored.replay) return send(res, 202, stored.value);
      const accepted = {
        message_id: crypto.randomUUID(),
        turn_id: crypto.randomUUID(),
        session_id: SESSION,
      };
      state.messageKeys.set(String(key), { hash, value: accepted });
      const commandId = crypto.randomUUID();
      state.live.messages.push(
        { seq: state.live.nextSeq, id: accepted.message_id, role: 'user', text: body.message, turn_id: accepted.turn_id, turn_status: 'queued' },
        { seq: state.live.nextSeq + 1, id: crypto.randomUUID(), role: 'action', text: '已接受用户回合并生成命令', turn_id: accepted.turn_id, turn_status: 'running', command_id: commandId, work_item_id: WORK[0] },
      );
      state.live.nextSeq += 2;
      bump();
      return send(res, 202, accepted);
    }
    const commands = path.match(/^\/api\/runs\/([^/]+)\/commands$/);
    if (commands && req.method === 'POST') {
      await new Promise((resolve) => setTimeout(resolve, 700));
      const key = req.headers['idempotency-key'];
      if (!key) return send(res, 400, envelope('invalid_input', 'Idempotency-Key is required'));
      const body = await readBody(req);
      if (body.role) return send(res, 400, envelope('invalid_input', 'command JSON must not include role'));
      if (commands[1] === RUN && body.expected_epoch !== state.live.epoch) {
        return send(res, 409, envelope('epoch_conflict', 'expected_epoch 与快照不一致', { scope: 'run', suggested_action: '读取快照后重试' }));
      }
      const hash = JSON.stringify(body);
      const prior = state.commandKeys.get(String(key));
      if (prior && prior.hash !== hash) return send(res, 409, envelope('idempotency_conflict', 'Idempotency-Key 已用于不同命令', { suggested_action: '使用新的 Idempotency-Key' }));
      if (prior) return send(res, 202, prior.value);
      if (commands[1] === RUN) {
        if (body.type === 'kill_work') {
          const target = state.live.directions.find((item) => item.work_item_id === body.work_item_id);
          if (target) target.status = 'killed';
        } else if (body.type === 'pause_run') state.live.status = 'paused';
        else if (body.type === 'resume_run') state.live.status = 'running';
        else if (body.type === 'stop_run') state.live.status = 'stopped';
        else if (body.type === 'start_epoch') {
          state.live.epoch += 1;
          state.live.status = 'running';
        } else if (body.type === 'grant_budget') {
          state.live.budget = { ...state.live.budget, ...body.payload };
        }
        bump();
      }
      const result = {
        command_id: crypto.randomUUID(),
        status: 'accepted',
        type: body.type,
        epoch_number: commands[1] === RUN ? state.live.epoch : 2,
        work_item_id: body.work_item_id,
        delivered: false,
      };
      state.commandKeys.set(String(key), { hash, value: result });
      return send(res, 202, result);
    }
    const model = path.match(/^\/api\/runs\/([^/]+)\/model_calls$/);
    if (model && req.method === 'GET') return send(res, 200, { model_calls: model[1] === RUN ? modelCalls() : [] });
    const trace = path.match(/^\/api\/runs\/([^/]+)\/work_items\/([^/]+)\/trace$/);
    if (trace && req.method === 'GET') {
      const tool = trace[2] === WORK[1] ? 'browser_snapshot' : 'http_request';
      return send(res, 200, { steps: [{ step_id: `step-${trace[2].slice(0, 8)}`, tool_name: tool, status: 'succeeded', summary: `${tool} response saved` }] });
    }
    const sessions = path.match(/^\/api\/runs\/([^/]+)\/sessions$/);
    if (sessions && req.method === 'GET') {
      return send(res, 200, { sessions: [
        { id: SESSION, work_item_id: WORK[0], attempt_id: ATTEMPTS[0], status: 'running' },
        { id: '44444444-4444-4444-8444-444444444442', work_item_id: WORK[0], attempt_id: '33333333-3333-4333-8333-333333333339', status: 'completed' },
      ] });
    }
    const artifactItem = path.match(/^\/api\/runs\/([^/]+)\/artifacts\/([^/]+)$/);
    if (artifactItem && req.method === 'GET') {
      return send(res, 200, { id: artifactItem[2], content: 'fixture-body HTTP/1.1 200', offset: 0, has_more: false });
    }
    const artifacts = path.match(/^\/api\/runs\/([^/]+)\/artifacts$/);
    if (artifacts && req.method === 'GET') {
      return send(res, 200, { artifacts: [
        { id: ARTIFACT, name: 'entry-response.txt', work_item_id: WORK[0] },
        { id: ARTIFACT_OTHER, name: 'other-direction.txt', work_item_id: WORK[1] },
      ], has_more: false });
    }
    const facts = path.match(/^\/api\/runs\/([^/]+)\/facts$/);
    if (facts && req.method === 'GET') return send(res, 200, { facts: [{ id: FACT, state: 'observed', payload: { summary: '入口返回 200' } }] });
    const findingList = path.match(/^\/api\/runs\/([^/]+)\/findings$/);
    if (findingList && req.method === 'GET') return send(res, 200, { findings: findings() });
    const goals = path.match(/^\/api\/runs\/([^/]+)\/goals$/);
    if (goals && req.method === 'GET') return send(res, 200, { goals: [{ id: GOAL, state: 'open', payload: { text: '确认授权范围内的入口行为' } }] });
    const items = path.match(/^\/api\/runs\/([^/]+)\/work_items$/);
    if (items && req.method === 'GET') {
      if (items[1] === LEGACY) return send(res, 200, { work_items: [{ id: WORK[0], title: '历史入口检查', status: 'completed' }] });
      return send(res, 200, { work_items: [] });
    }
    const checks = path.match(/^\/api\/runs\/([^/]+)\/checks$/);
    if (checks && req.method === 'GET') return send(res, 200, { check_results: [] });
    const report = path.match(/^\/api\/runs\/([^/]+)\/finding-report$/);
    if (report && req.method === 'GET') {
      return send(res, 200, { run_id: report[1], generated_at: state.live.snapshotAt, summary: { total: 1, pending_review: 0, confirmed: 1, false_positive: 0, ignored: 0 }, findings: [] });
    }
    return send(res, 404, { error: 'not found' });
  } catch (err) {
    return send(res, 500, envelope('fixture_error', err instanceof Error ? err.message : 'fixture error', { retryable: true }));
  }
});

server.listen(PORT, '127.0.0.1', () => {
  process.stdout.write(`workbench fixture listening on ${PORT}\n`);
});
