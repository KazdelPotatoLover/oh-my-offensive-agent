import assert from 'node:assert/strict';
import test from 'node:test';
import type { Run } from '../types';
import { runNotice } from './run-notice';

const run = (fields: Partial<Run>): Run => ({
  id: 'run', task_id: 'task', current_epoch: 1, config_snapshot: {},
  status: 'stopped', created_at: '', updated_at: '', ...fields,
});

test('temporary provider stop exposes the persisted recovery explanation', () => {
  const notice = runNotice(run({ finish_reason: 'needs_input', final_output: '模型服务暂时不可用\n\nEvidence inventory' }));
  assert.equal(notice?.detail, '模型服务暂时不可用');
  assert.match(runNotice(run({ status: 'failed', finish_reason: 'provider_error' }))!.detail, /服务恢复/);
  assert.equal(runNotice(run({ status: 'completed', finish_reason: 'completed' })), null);
});

test('exhausted allowance explains that continuation preserves usage', () => {
  const notice = runNotice(run({ finish_reason: 'worker_budget_reserved' }));
  assert.match(notice!.detail, /追加/);
  assert.match(notice!.detail, /不会清零/);
});
