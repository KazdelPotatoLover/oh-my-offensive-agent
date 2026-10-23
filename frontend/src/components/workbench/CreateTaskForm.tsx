'use client';

import React, { useEffect, useState } from 'react';
import { useRouter } from 'next/navigation';
import { api, newRequestID } from '@/lib/api';
import { buildAgentsConfig, buildDiscoveryScope, buildTaskConfig } from '@/lib/workbench/budget';
import { creationBudget, creationName, durationSummary, unlimitedTimeoutPolicy, type CreationDefaults, type DurationMode } from '@/lib/workbench/creation';
import { workbenchHref } from '@/lib/workbench/links';

const fieldClass = 'w-full rounded-md border border-gray-700 bg-gray-950 px-2.5 py-1.5 text-sm text-gray-100 focus:outline-none focus:ring-2 focus:ring-blue-500';

export function CreateTaskForm({ onCancel }: { onCancel: () => void }) {
  const router = useRouter();
  const [name, setName] = useState('');
  const [objective, setObjective] = useState('');
  const [origin, setOrigin] = useState('');
  const [seedPath, setSeedPath] = useState('');
  const [provider, setProvider] = useState<'mock' | 'openai'>('openai');
  const [profile, setProfile] = useState('');
  const [model, setModel] = useState('');
  const [enableCommands, setEnableCommands] = useState(false);
  const [profilesText, setProfilesText] = useState('');
  const [workers, setWorkers] = useState('');
  const [minutes, setMinutes] = useState('');
  const [durationMode, setDurationMode] = useState<DurationMode>('inherit');
  const [defaults, setDefaults] = useState<CreationDefaults | null>(null);
  const [defaultsError, setDefaultsError] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  useEffect(() => {
    let cancelled = false;
    api.getCreationDefaults().then(value => {
      if (!cancelled) setDefaults(value);
    }).catch(() => { if (!cancelled) setDefaultsError(true); });
    return () => { cancelled = true; };
  }, []);

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);
    if (!objective.trim()) {
      setError('请填写任务目标。');
      return;
    }
    const scope = buildDiscoveryScope({ origin: origin.trim(), seedPath });
    if (!scope.ok) {
      setError(scope.error);
      return;
    }
    try {
      const budget = creationBudget(workers, minutes);
      const executionPolicy = unlimitedTimeoutPolicy(durationMode, minutes);
      const toolNames = ['get_endpoint', 'http_request', 'read_sample', 'extract_endpoint_links', 'extract_api_endpoints', 'run_security_check', 'record_observation', 'record_observation_batch', 'inspect_artifact', 'register_asset', 'search_http_traffic', 'get_http_exchange', 'list_skills', 'read_skill'];
      if (enableCommands) toolNames.push('run_command', 'write_file', 'read_file', 'inspect_command');
      let profiles: unknown[] = [{ name: 'explore', version: '1', tools: toolNames.map(name => ({ name, version: '1' })) }];
      if (profilesText.trim()) {
        try { profiles = JSON.parse(profilesText); } catch { throw new Error('能力 profile 不是合法 JSON。'); }
        if (!Array.isArray(profiles)) throw new Error('能力 profile 必须是数组。');
      }
      const agents = model.trim() || profile.trim() ? buildAgentsConfig(model.trim(), profile.trim()) : undefined;
      const budgetOverride = Object.keys(budget).length ? budget : undefined;
      setPending(true);
      const task = await api.createTask({
        name: creationName(name, objective),
        objective: objective.trim(),
        scope: scope.scope,
        config: buildTaskConfig({ provider, profiles, agents, budget: budgetOverride, executionPolicy, mockScenario: provider === 'mock' ? 'orchestration' : undefined }),
      });
      const run = await api.createRun(task.id, {
        executor: 'orchestrator', provider,
        mock_scenario: provider === 'mock' ? 'orchestration' : undefined,
        idempotency_key: newRequestID(),
        budget: budgetOverride,
        execution_policy: executionPolicy,
      });
      router.push(workbenchHref({ taskId: task.id, runId: run.id }));
    } catch (err) {
      setError(err instanceof Error ? err.message : '创建失败');
      setPending(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-4" data-testid="create-task-form" aria-labelledby="create-task-heading">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 id="create-task-heading" className="text-base font-semibold text-white">创建探索任务</h2>
          <p className="mt-1 text-xs text-gray-400">填写目标和网站地址，模型与执行额度自动使用服务器设置。</p>
        </div>
        <button type="button" onClick={onCancel} className="text-xs text-gray-400 hover:text-white">关闭</button>
      </div>
      {error && <p role="alert" data-testid="create-error" className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">{error}</p>}
      <label className="block text-xs text-gray-300">任务目标
        <textarea id="create-task-objective" required value={objective} onChange={event => setObjective(event.target.value)} rows={3} placeholder="例如：检查网站的安全配置与暴露接口，保存证据并给出加固建议" className={`${fieldClass} mt-1`} />
      </label>
      <label className="block text-xs text-gray-300">授权网站地址
        <input id="scope-origin" required type="url" value={origin} onChange={event => setOrigin(event.target.value)} placeholder="https://example.com/" className={`${fieldClass} mt-1`} />
      </label>
      <p className="text-[11px] text-gray-400">支持带路径的网址；仅访问该网站的同源地址，默认允许 GET/HEAD。</p>
      <div data-testid="creation-defaults" className="rounded-md border border-gray-800 bg-gray-900/50 px-3 py-2 text-xs text-gray-400">
        {provider === 'mock' ? '本地演示模式 · 不调用真实模型' : '使用服务器模型'}
        {defaults && <> · {workers || defaults.budget.max_parallel_workers} 个并发 Worker · {durationSummary(minutes, durationMode, defaults)}</>}
        <p className="mt-1">单个方向额度耗尽后由 Planner 判断下一步；无需逐项分配预算。</p>
        {defaultsError && <p className="mt-1 text-amber-300">默认设置暂时无法读取，创建时将由服务器解析。</p>}
      </div>
      {defaults?.model_configured === false && provider === 'openai' && (
        <p role="alert" className="text-xs text-amber-300">服务器尚未配置模型，请先完成模型连接设置；本地演示可在高级设置中选择。</p>
      )}
      <details className="rounded-md border border-gray-800 p-3" data-testid="creation-advanced">
        <summary className="cursor-pointer text-xs text-gray-300">高级设置（可选）</summary>
        <div className="mt-3 space-y-3">
          <label className="block text-xs text-gray-300">任务名（可选）
            <input id="create-task-name" value={name} onChange={event => setName(event.target.value)} placeholder="留空时根据目标生成" className={`${fieldClass} mt-1`} />
          </label>
          <div className="grid gap-2 sm:grid-cols-2">
            <label className="block text-xs text-gray-300">并发 Worker 数
              <input id="worker-count" type="number" min={1} max={8} step={1} value={workers} onChange={event => setWorkers(event.target.value)} placeholder="继承服务器设置" className={`${fieldClass} mt-1`} />
            </label>
            <label className="block text-xs text-gray-300">运行时长（分钟）
              <input id="run-minutes" type="number" min={1} max={1440} step={1} value={minutes} onChange={event => setMinutes(event.target.value)} placeholder="继承服务器设置" className={`${fieldClass} mt-1`} />
            </label>
          </div>
          <label className="block text-xs text-gray-300">任务时长
            <select id="duration-mode" value={durationMode} onChange={event => setDurationMode(event.target.value as DurationMode)} className={`${fieldClass} mt-1`}>
              <option value="inherit">继承服务器设置</option>
              <option value="unlimited">不限时</option>
            </select>
          </label>
          <p className="text-[11px] text-gray-500">留空并选择继承，使用服务器设置。选择不限时会覆盖全局期限。填写分钟数则使用该时长。0 不表示不限。</p>
          <label className="flex items-center gap-2 text-xs text-gray-300"><input type="checkbox" checked={enableCommands} onChange={event => setEnableCommands(event.target.checked)} />启用隔离命令与脚本</label>
          <details className="border-t border-gray-800 pt-2">
            <summary className="cursor-pointer text-xs text-gray-400">开发与调试</summary>
            <div className="mt-3 space-y-3">
              <label className="block text-xs text-gray-300">执行模式
                <select aria-label="执行模式" value={provider} onChange={event => setProvider(event.target.value as 'mock' | 'openai')} className={`${fieldClass} mt-1`}>
                  <option value="openai">服务器模型（默认）</option>
                  <option value="mock">本地演示（不调用真实模型）</option>
                </select>
              </label>
              <label className="block text-xs text-gray-300">种子路径覆盖（可选）
                <input id="seed-path" value={seedPath} onChange={event => setSeedPath(event.target.value)} placeholder="默认使用网站地址中的路径" className={`${fieldClass} mt-1`} />
              </label>
              <label className="block text-xs text-gray-300">runtime_profile（可留空）
                <input aria-label="runtime_profile" value={profile} onChange={event => setProfile(event.target.value)} className={`${fieldClass} mt-1`} />
              </label>
              <label className="block text-xs text-gray-300">model 别名（可留空）
                <input aria-label="model 别名" value={model} onChange={event => setModel(event.target.value)} className={`${fieldClass} mt-1`} />
              </label>
              <label className="block text-xs text-gray-300">能力 profile JSON（可留空）
                <textarea aria-label="能力 profile 配置" value={profilesText} onChange={event => setProfilesText(event.target.value)} rows={2} className={`${fieldClass} mt-1 font-mono`} />
              </label>
            </div>
          </details>
        </div>
      </details>
      <button type="submit" disabled={pending || (defaults?.model_configured === false && provider === 'openai')} data-testid="create-task-submit" className="rounded-md bg-blue-600 px-3 py-2 text-sm font-medium text-white disabled:opacity-50">
        {pending ? '正在创建' : '创建并启动'}
      </button>
    </form>
  );
}
