'use client';

import React, { useState } from 'react';
import { useRouter } from 'next/navigation';
import { api, newRequestID } from '@/lib/api';
import { EXPLORATION_TEMPLATE, QUICK_TEMPLATE, buildAgentsConfig, buildDiscoveryScope, buildRunBudget, buildTaskConfig, validateWorkerCount, type BudgetTemplate } from '@/lib/workbench/budget';
import { workbenchHref } from '@/lib/workbench/links';

const fieldClass = 'w-full rounded-md border border-gray-700 bg-gray-950 px-2.5 py-1.5 text-sm text-gray-100 focus:outline-none focus:ring-2 focus:ring-blue-500';

export function CreateTaskForm({ onCancel }: { onCancel: () => void }) {
  const router = useRouter();
  const [name, setName] = useState('');
  const [objective, setObjective] = useState('');
  const [origin, setOrigin] = useState('');
  const [seedPath, setSeedPath] = useState('/');
  const [provider, setProvider] = useState<'mock' | 'openai'>('mock');
  const [profile, setProfile] = useState('');
  const [model, setModel] = useState('');
  const [enableCommands, setEnableCommands] = useState(false);
  const [profilesText, setProfilesText] = useState('');
  const [template, setTemplate] = useState<BudgetTemplate>(EXPLORATION_TEMPLATE);
  const [workers, setWorkers] = useState(EXPLORATION_TEMPLATE.max_parallel_workers);
  const [budget, setBudget] = useState<BudgetTemplate>(EXPLORATION_TEMPLATE);
  const [error, setError] = useState<string | null>(null);
  const [pending, setPending] = useState(false);

  const chooseTemplate = (next: BudgetTemplate) => {
    setTemplate(next);
    setBudget(next);
    setWorkers(next.max_parallel_workers);
  };

  const editBudget = (key: keyof BudgetTemplate, value: number) => {
    setBudget((current) => ({ ...current, [key]: value }));
  };

  const submit = async (event: React.FormEvent) => {
    event.preventDefault();
    const workerError = validateWorkerCount(workers);
    if (!name.trim() || !objective.trim()) {
      setError('任务名和目标都要填写。');
      return;
    }
    if (workerError) {
      setError(workerError);
      return;
    }
    const scope = buildDiscoveryScope({ origin: origin.trim(), seedPath: seedPath.trim() || '/' });
    if (!scope.ok) {
      setError(scope.error);
      return;
    }
    // New tasks explicitly authorize the richer toolset; old snapshots stay intact.
    const toolNames = ['get_endpoint', 'http_request', 'read_sample', 'extract_endpoint_links', 'record_observation', 'record_observation_batch', 'inspect_artifact', 'register_asset'];
    if (enableCommands) toolNames.push('run_command', 'write_file', 'read_file', 'inspect_command');
    let profiles: unknown[] = [{ name: 'explore', version: '1', tools: toolNames.map((name) => ({ name, version: '1' })) }];
    if (profilesText.trim()) {
      try {
        profiles = JSON.parse(profilesText);
      } catch {
        setError('能力 profile 不是合法 JSON。');
        return;
      }
      if (!Array.isArray(profiles)) {
        setError('能力 profile 必须是数组。');
        return;
      }
    }
    let agents;
    try {
      if (model.trim() || profile.trim()) agents = buildAgentsConfig(model.trim(), profile.trim());
    } catch (err) {
      setError(err instanceof Error ? err.message : '角色配置无效');
      return;
    }
    const granted = buildRunBudget({ ...budget, name: template.name }, workers);
    const { name: _templateName, ...budgetFields } = granted;
    setPending(true);
    setError(null);
    try {
      const task = await api.createTask({
        name: name.trim(),
        objective: objective.trim(),
        scope: scope.scope,
        config: buildTaskConfig({ provider, profiles, agents, budget: budgetFields, mockScenario: provider === 'mock' ? 'orchestration' : undefined }),
      });
      const run = await api.createRun(task.id, {
        executor: 'orchestrator',
        provider,
        mock_scenario: provider === 'mock' ? 'orchestration' : undefined,
        idempotency_key: newRequestID(),
        budget: budgetFields,
      });
      router.push(workbenchHref({ taskId: task.id, runId: run.id }));
    } catch (err) {
      setError(err instanceof Error ? err.message : '创建失败');
      setPending(false);
    }
  };

  return (
    <form onSubmit={submit} className="space-y-3" data-testid="create-task-form" aria-labelledby="create-task-heading">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h2 id="create-task-heading" className="text-base font-semibold text-white">创建探索任务</h2>
          <p className="text-xs text-gray-400">填写目标和授权范围即可开始；模型默认使用服务器配置。</p>
        </div>
        <button type="button" onClick={onCancel} className="text-xs text-gray-400 hover:text-white">关闭</button>
      </div>
      {error && <p role="alert" data-testid="create-error" className="rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-sm text-red-200">{error}</p>}
      <label className="block text-xs text-gray-300">任务名
        <input id="create-task-name" value={name} onChange={(event) => setName(event.target.value)} className={`${fieldClass} mt-1`} />
      </label>
      <label className="block text-xs text-gray-300">目标
        <textarea id="create-task-objective" value={objective} onChange={(event) => setObjective(event.target.value)} rows={3} className={`${fieldClass} mt-1`} />
      </label>
      <div className="grid gap-2 sm:grid-cols-2">
        <label className="block text-xs text-gray-300">授权 origin
          <input id="scope-origin" value={origin} onChange={(event) => setOrigin(event.target.value)} placeholder="http://127.0.0.1:29090" className={`${fieldClass} mt-1`} />
        </label>
        <label className="block text-xs text-gray-300">种子路径
          <input id="seed-path" value={seedPath} onChange={(event) => setSeedPath(event.target.value)} className={`${fieldClass} mt-1`} />
        </label>
      </div>
      <p className="text-[11px] text-gray-400">只访问填写的 origin，允许 GET/HEAD；Worker 数量按下方配置执行。</p>
      <fieldset className="space-y-2">
        <legend className="text-xs text-gray-300">模型配置</legend>
        <div className="flex gap-2">
          <button type="button" onClick={() => setProvider('mock')} className={`rounded-md border px-2 py-1 text-xs ${provider === 'mock' ? 'border-amber-400 text-amber-200' : 'border-gray-700 text-gray-400'}`}>本地协议替身</button>
          <button type="button" onClick={() => setProvider('openai')} className={`rounded-md border px-2 py-1 text-xs ${provider === 'openai' ? 'border-blue-400 text-blue-200' : 'border-gray-700 text-gray-400'}`}>真实模型 API</button>
        </div>
        <div className="grid gap-2 sm:grid-cols-2">
          <label className="text-xs text-gray-300">runtime_profile（可留空）
            <input aria-label="runtime_profile" value={profile} onChange={(event) => setProfile(event.target.value)} className={`${fieldClass} mt-1`} />
          </label>
          <label className="text-xs text-gray-300">model 别名（可留空）
            <input aria-label="model 别名" value={model} onChange={(event) => setModel(event.target.value)} className={`${fieldClass} mt-1`} />
          </label>
        </div>
      </fieldset>
      <label className="flex items-center gap-2 text-xs text-gray-300"><input type="checkbox" checked={enableCommands} onChange={(event) => setEnableCommands(event.target.checked)} />启用隔离命令与脚本（需要服务器执行代理）</label>
      <label className="block text-xs text-gray-300">能力 profile JSON（可留空）
        <textarea aria-label="能力 profile 配置" value={profilesText} onChange={(event) => setProfilesText(event.target.value)} rows={2} className={`${fieldClass} mt-1 font-mono`} placeholder='[{"name":"browser_evidence","version":"1","tools":[{"name":"get_endpoint"}]}]' />
      </label>
      <fieldset className="space-y-2">
        <legend className="text-xs text-gray-300">预算模板</legend>
        <div className="flex gap-2">
          <button type="button" onClick={() => chooseTemplate(EXPLORATION_TEMPLATE)} className={`rounded-md border px-2 py-1 text-xs ${template.name === 'exploration' ? 'border-blue-400 text-blue-200' : 'border-gray-700 text-gray-400'}`}>探索任务</button>
          <button type="button" onClick={() => chooseTemplate(QUICK_TEMPLATE)} className={`rounded-md border px-2 py-1 text-xs ${template.name === 'quick_verification' ? 'border-blue-400 text-blue-200' : 'border-gray-700 text-gray-400'}`}>快速验证</button>
        </div>
        <label className="block text-xs text-gray-300">Worker 池 N
          <input id="worker-count" type="number" min={1} max={8} step={1} value={workers} onChange={(event) => setWorkers(Number(event.target.value))} className={`${fieldClass} mt-1`} />
        </label>
        <div className="grid grid-cols-2 gap-2">
          {([
            ['max_model_calls', '总模型次数'],
            ['max_tool_calls', '总工具次数'],
            ['run_timeout_seconds', '总时长秒'],
            ['worker_max_model_calls', '方向模型次数'],
            ['worker_max_tool_calls', '方向工具次数'],
            ['worker_timeout_seconds', '方向时长秒'],
            ['planner_reserved_model_calls', 'Planner 预留'],
            ['main_reserved_model_calls', 'Main 预留'],
          ] as const).map(([key, label]) => (
            <label key={key} className="text-[11px] text-gray-400">{label}
              <input aria-label={label} type="number" value={budget[key]} onChange={(event) => editBudget(key, Number(event.target.value))} className={`${fieldClass} mt-1`} />
            </label>
          ))}
        </div>
      </fieldset>
      <button type="submit" disabled={pending} data-testid="create-task-submit" className="rounded-md bg-blue-600 px-3 py-2 text-sm font-medium text-white disabled:opacity-50">
        {pending ? '正在创建' : '创建并启动'}
      </button>
    </form>
  );
}
