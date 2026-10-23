import assert from 'node:assert/strict';
import test from 'node:test';
import { creationBudget, creationName, durationSummary, unlimitedTimeoutPolicy } from './creation';
import { budgetSummaryText, buildTaskConfig } from './budget';
import { buildDiscoveryScope } from './budget';

test('ordinary creation inherits defaults and derives its name from the objective', () => {
  assert.deepEqual(creationBudget('', ''), {});
  const config = buildTaskConfig({ provider: 'openai', profiles: [], budget: creationBudget('', '') });
  assert.equal('budget' in config, false);
  assert.equal('agents' in config, false);
  assert.equal(config.provider, 'openai');
  assert.equal(creationName('', '  检查网站安全\n保存证据  '), '检查网站安全');
  assert.equal(creationName(' 自定名称 ', '目标'), '自定名称');
});

test('overrides send only chosen settings, use minutes, and do not interpret zero as unlimited', () => {
  assert.deepEqual(creationBudget('5', ''), { max_parallel_workers: 5 });
  assert.deepEqual(creationBudget('', '45'), { run_timeout_seconds: 2700 });
  assert.deepEqual(creationBudget('2', '10'), { max_parallel_workers: 2, run_timeout_seconds: 600 });
  for (const n of ['0', '9', '1.5', 'NaN']) assert.throws(() => creationBudget(n, ''));
  for (const n of ['0', '-1', '1.5', 'Infinity', '1441']) assert.throws(() => creationBudget('', n));
});

test('duration display distinguishes inherit, unlimited and explicit minutes', () => {
  const unlimited = { budget: { max_parallel_workers: 3, run_timeout_seconds: 0 }, limits: { task_timeout_limited: false }, model_configured: true };
  const bounded = { budget: { max_parallel_workers: 5, run_timeout_seconds: 3600 }, model_configured: false };
  assert.equal(durationSummary('', 'inherit', unlimited), '不限时');
  assert.equal(durationSummary('', 'inherit', bounded), '最长 60 分钟');
  assert.equal(durationSummary('', 'unlimited', bounded), '不限时');
  assert.equal(durationSummary('45', 'inherit', unlimited), '最长 45 分钟');
  assert.equal(durationSummary('0', 'inherit', unlimited), '不限时');
  assert.equal(unlimitedTimeoutPolicy('inherit', ''), undefined);
  assert.deepEqual(unlimitedTimeoutPolicy('unlimited', ''), { task_timeout_seconds: 0 });
  assert.equal(unlimitedTimeoutPolicy('unlimited', '10'), undefined);
  const config = buildTaskConfig({ provider: 'openai', profiles: [], executionPolicy: unlimitedTimeoutPolicy('unlimited', '') });
  assert.deepEqual(config.execution_policy, { task_timeout_seconds: 0 });
  assert.equal(budgetSummaryText({ max_model_calls: 0, max_tool_calls: 0, run_timeout_seconds: 0, limits: { model_calls_limited: false, tool_calls_limited: false, task_timeout_limited: false } }), '模型 不设上限 · 工具 不设上限 · 时长 不限时');
  assert.equal(budgetSummaryText({ max_model_calls: 30, max_tool_calls: 12, run_timeout_seconds: 120 }), '模型 30 · 工具 12 · 时长 120 秒');
});

test('a full target URL keeps its path/query while the authority remains same-origin', () => {
  const result = buildDiscoveryScope({ origin: 'https://example.com/app/?view=1', seedPath: '' });
  assert.ok(result.ok);
  if (!result.ok) return;
  assert.equal(result.scope.seeds[0], 'https://example.com/app/?view=1');
  assert.equal(result.scope.discovery_policy.origin, 'https://example.com');
  assert.equal(result.scope.network_policy.template, 'active');
  assert.deepEqual(result.scope.network_policy.allowed_methods, ['GET', 'HEAD', 'OPTIONS', 'POST', 'PUT', 'PATCH', 'DELETE']);
  const readonly = buildDiscoveryScope({ origin: 'https://example.com/app/?view=1', seedPath: '', networkMode: 'readonly' });
  assert.ok(readonly.ok);
  assert.equal(readonly.scope.network_policy.template, 'readonly');
  assert.deepEqual(readonly.scope.network_policy.allowed_methods, ['GET', 'HEAD']);
  assert.deepEqual(readonly.scope.seeds, result.scope.seeds);
  assert.equal(buildDiscoveryScope({ origin: 'https://user:password@example.com/', seedPath: '' }).ok, false);
});
