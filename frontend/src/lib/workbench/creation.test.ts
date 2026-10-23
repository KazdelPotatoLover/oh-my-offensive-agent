import assert from 'node:assert/strict';
import test from 'node:test';
import { creationBudget, creationName } from './creation';
import { buildDiscoveryScope, buildTaskConfig } from './budget';

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

test('a full target URL keeps its path/query while the authority remains same-origin', () => {
  const result = buildDiscoveryScope({ origin: 'https://example.com/app/?view=1', seedPath: '' });
  assert.ok(result.ok);
  if (!result.ok) return;
  assert.equal(result.scope.seeds[0], 'https://example.com/app/?view=1');
  assert.equal(result.scope.discovery_policy.origin, 'https://example.com');
  assert.deepEqual(result.scope.network_policy.allowed_methods, ['GET', 'HEAD']);
  assert.equal(buildDiscoveryScope({ origin: 'https://user:password@example.com/', seedPath: '' }).ok, false);
});
