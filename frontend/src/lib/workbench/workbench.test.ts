import assert from 'node:assert/strict';
import test from 'node:test';
import { EXPLORATION_TEMPLATE, QUICK_TEMPLATE, buildAgentsConfig, buildDiscoveryScope, buildRunBudget, droppedBudgetFields, validateWorkerCount } from './budget';
import { buildCommand, classifyCommandResponse, commandsPath, mainMessagesPath, readCommandResult, readMainMessages, readSnapshot, workbenchSnapshotPath } from './client';
import { parseMainMessagePage, parseWorkbenchSnapshot, type WorkbenchSnapshot } from './parse';
import { workbenchHref, selectionFromPath } from './links';
import { appendMessages, failLoad, initialLoadState, resetLoad, succeedLoad } from './state';

const RUN = '11111111-1111-4111-8111-111111111111';
const WORK = '22222222-2222-4222-8222-222222222222';
const ATTEMPT = '33333333-3333-4333-8333-333333333333';
const SESSION = '44444444-4444-4444-8444-444444444444';

function snapshot(overrides: Record<string, unknown> = {}): WorkbenchSnapshot {
  return {
    schema_version: 'workbench.v1',
    snapshot_at: '2026-10-22T00:00:00Z',
    run: { id: RUN, status: 'running', epoch_number: 2 },
    worker_capacity: { requested: 4, effective: 4, active: 2, queued: 3 },
    model_capacity: { limit: 2, in_flight: 2, queued: 1 },
    budget: {},
    goals: [],
    planner: { status: 'waiting', reason: '等待当前方向写回', session_id: SESSION },
    directions: [
      {
        work_item_id: WORK,
        objective: '读取授权入口并保存响应',
        status: 'running',
        attempt_id: ATTEMPT,
        session_id: SESSION,
        latest_step: 'http_request',
        allowed_actions: ['steer_work', 'kill_work'],
      },
    ],
    main_conversation: { last_seq: 12, active_turn_id: null },
    events: [],
    last_event_seq: 123,
    has_more_events: false,
    ...overrides,
  };
}

test('workbench.v1 parser accepts the frozen example and rejects a missing directions array', () => {
  const parsed = parseWorkbenchSnapshot(snapshot());
  assert.equal(parsed.schema_version, 'workbench.v1');
  assert.equal(parsed.worker_capacity.requested, 4);
  assert.equal(parsed.directions[0].latest_step, 'http_request');
  assert.throws(() => parseWorkbenchSnapshot(snapshot({ directions: undefined })), /directions/);
});

test('model calls are not a substitute for worker cards or capacity', () => {
  const parsed = parseWorkbenchSnapshot(snapshot({
    worker_capacity: { requested: 4, effective: 4, active: 2, queued: 1 },
    directions: [
      { work_item_id: WORK, objective: 'a', status: 'running', latest_step: 'http_request', allowed_actions: ['kill_work'] },
      { work_item_id: '55555555-5555-4555-8555-555555555555', objective: 'b', status: 'queued', latest_step: 'claim', allowed_actions: [] },
    ],
  }));
  assert.equal(parsed.directions.length, 2);
  assert.equal(parsed.worker_capacity.active, 2);
  assert.notEqual(parsed.directions.length, parsed.worker_capacity.requested);
  assert.throws(() => parseWorkbenchSnapshot({ ...snapshot(), model_calls: [{ id: '1' }, { id: '2' }, { id: '3' }, { id: '4' }], directions: undefined }), /directions/);
});

test('snapshot and message errors are not rewritten into empty collections', async () => {
  const fetchImpl = async () => new Response(JSON.stringify({
    error: { code: 'not_implemented', scope: 'request', message: 'contract port has no production implementation', retryable: false, suggested_action: 'none' },
  }), { status: 501 });
  await assert.rejects(() => readSnapshot(fetchImpl, RUN), (error: { status?: number; code?: string }) => {
    assert.equal(error.status, 501);
    assert.equal(error.code, 'not_implemented');
    return true;
  });
});

test('load state keeps the last snapshot on failure and drops it when the run changes', () => {
  const first = succeedLoad(initialLoadState<WorkbenchSnapshot>(), 1, snapshot());
  const failed = failLoad(first, 1, { status: 500, code: 'persistence_error', message: 'read failed' });
  assert.equal(failed.data?.directions.length, 1);
  assert.equal(failed.stale, true);
  assert.equal(failed.error?.message, 'read failed');
  const reset = resetLoad<WorkbenchSnapshot>(2);
  assert.equal(reset.data, null);
  const staleWrite = succeedLoad(reset, 1, snapshot());
  assert.equal(staleWrite.data, null);
  const next = succeedLoad(reset, 2, snapshot({ run: { id: '66666666-6666-4666-8666-666666666666', status: 'paused', epoch_number: 2 } }));
  assert.equal(next.data?.run.status, 'paused');
});

test('message pages dedupe by seq and a failed page does not clear them', () => {
  const page = parseMainMessagePage({
    messages: [
      { seq: 1, id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa', role: 'user', text: '暂停另一个方向', turn_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb', turn_status: 'completed' },
      { seq: 2, id: 'cccccccc-cccc-4ccc-8ccc-cccccccccccc', role: 'action', text: 'kill_work', command_id: 'dddddddd-dddd-4ddd-8ddd-dddddddddddd', work_item_id: WORK },
    ],
    last_seq: 2,
    has_more: false,
  });
  const merged = appendMessages([], page);
  const again = appendMessages(merged, page);
  assert.equal(again.length, 2);
  assert.equal(again[1].command_id, 'dddddddd-dddd-4ddd-8ddd-dddddddddddd');
  assert.throws(() => parseMainMessagePage({
    messages: [{ seq: 3, id: 'eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee', role: 'action', text: 'done' }],
    last_seq: 3,
    has_more: false,
  }), /action/);
});

test('commands use the frozen route, epoch and idempotency classification', async () => {
  assert.equal(commandsPath(RUN), `/api/runs/${RUN}/commands`);
  assert.equal(workbenchSnapshotPath(RUN, 4, 50), `/api/runs/${RUN}/workbench?after_event_seq=4&limit=50`);
  assert.equal(mainMessagesPath(RUN, 12, 50), `/api/runs/${RUN}/main/messages?after_seq=12&limit=50`);
  const command = buildCommand({ type: 'kill_work', expectedEpoch: 2, workItemId: WORK, expectedAttemptId: ATTEMPT, payload: { reason: 'stop this direction' } });
  assert.equal(command.type, 'kill_work');
  assert.equal('role' in command, false);
  assert.equal(command.work_item_id, WORK);
  const conflict = classifyCommandResponse(409, {
    error: { code: 'idempotency_conflict', scope: 'request', message: 'same key, different body', retryable: false, suggested_action: 'none' },
  });
  assert.equal(conflict.phase, 'conflict');
  assert.equal(conflict.canResend, false);
  const dropped = classifyCommandResponse(0, null);
  assert.equal(dropped.phase, 'disconnected');
  assert.equal(dropped.canResend, true);
  const fetchImpl = async (_url: string, init?: RequestInit) => {
    assert.equal(new Headers(init?.headers).get('Idempotency-Key'), 'same-key');
    return new Response(JSON.stringify({
      command_id: 'ffffffff-ffff-4fff-8fff-ffffffffffff',
      status: 'accepted',
      type: 'kill_work',
      epoch_number: 2,
      work_item_id: WORK,
      attempt_id: ATTEMPT,
      delivered: false,
    }), { status: 202 });
  };
  const result = await readCommandResult(fetchImpl, RUN, command, 'same-key');
  assert.equal(result.httpStatus, 202);
  assert.equal(result.body.delivered, false);
  assert.equal(result.body.status, 'accepted');
});

test('main message submit records 202 and does not invent an assistant reply', async () => {
  const fetchImpl = async () => new Response(JSON.stringify({
    message_id: 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',
    turn_id: 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',
    session_id: SESSION,
  }), { status: 202 });
  const accepted = await readMainMessages.submit(fetchImpl, RUN, { message: '只终止入口方向', expected_epoch: 2 }, 'turn-key');
  assert.equal(accepted.httpStatus, 202);
  assert.equal(accepted.body.turn_id, 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb');
  assert.equal('text' in accepted.body, false);
});

test('new exploration tasks default to N=3 and reject values outside 1-8', () => {
  assert.equal(EXPLORATION_TEMPLATE.max_parallel_workers, 3);
  assert.equal(QUICK_TEMPLATE.max_parallel_workers, 1);
  assert.equal(validateWorkerCount(3), null);
  assert.match(validateWorkerCount(0) || '', /1 到 8/);
  assert.match(validateWorkerCount(9) || '', /1 到 8/);
  assert.equal(validateWorkerCount(1.5), 'Worker 池必须是 1 到 8 的整数');
  const budget = buildRunBudget(EXPLORATION_TEMPLATE, 4);
  assert.equal(budget.max_parallel_workers, 4);
  assert.equal(budget.max_model_calls, 80);
  assert.deepEqual(buildAgentsConfig('configured-model', 'default'), {
    mainagent: { runtime_profile: 'default', model: 'configured-model' },
    planner: { runtime_profile: 'default', model: 'configured-model' },
    worker: { runtime_profile: 'default', model: 'configured-model' },
    verifier: { runtime_profile: 'default', model: 'configured-model' },
  });
});

test('https seeds keep their scheme and explicit network grant, and dropped fields stay visible', () => {
  const scope = buildDiscoveryScope({ origin: 'https://127.0.0.1:29090', seedPath: '/' });
  assert.equal(scope.ok, true);
  if (scope.ok) {
    assert.equal(scope.scope.seeds[0], 'https://127.0.0.1:29090/');
    assert.equal(scope.scope.network_policy.template, 'active');
  }
  const httpScope = buildDiscoveryScope({ origin: 'http://127.0.0.1:29090', seedPath: '/login' });
  assert.equal(httpScope.ok, true);
  if (httpScope.ok) assert.equal((httpScope.scope.seeds as string[])[0], 'http://127.0.0.1:29090/login');
  const dropped = droppedBudgetFields(
    { max_parallel_workers: 4, planner_reserved_model_calls: 16, main_reserved_model_calls: 8 },
    { budget: { max_parallel_workers: 4, max_model_calls: 80 } },
  );
  assert.deepEqual(dropped, ['planner_reserved_model_calls', 'main_reserved_model_calls']);
});

test('deep links keep task, run and work item across refresh', () => {
  const href = workbenchHref({ taskId: 'task-1', runId: RUN, workItemId: WORK });
  assert.equal(href, `/workbench/tasks/task-1/runs/${RUN}/work/${WORK}`);
  assert.deepEqual(selectionFromPath(href, ''), { taskId: 'task-1', runId: RUN, workItemId: WORK });
  assert.deepEqual(selectionFromPath('/workbench', `?task=task-1&run=${RUN}&work=${WORK}`), {
    taskId: 'task-1',
    runId: RUN,
    workItemId: WORK,
  });

  // Task only selection parses and can be resolved to latest run
  const taskOnlyHref = workbenchHref({ taskId: 'task-1' });
  assert.equal(taskOnlyHref, '/workbench/tasks/task-1');
  assert.deepEqual(selectionFromPath(taskOnlyHref, ''), { taskId: 'task-1' });
  const resolvedRunHref = workbenchHref({ taskId: 'task-1', runId: RUN });
  assert.equal(resolvedRunHref, `/workbench/tasks/task-1/runs/${RUN}`);
});
