import { validateWorkerCount } from './budget';

export type CreationBudget = { max_parallel_workers?: number; run_timeout_seconds?: number };
export type DurationMode = 'inherit' | 'unlimited';
export type CreationDefaults = {
  budget: { max_parallel_workers: number; run_timeout_seconds: number };
  limits?: { task_timeout_limited?: boolean };
  model_configured: boolean;
};

export function durationSummary(minutes: string, mode: DurationMode, defaults: CreationDefaults | null): string {
  if (minutes.trim()) {
    const n = Number(minutes);
    if (Number.isSafeInteger(n) && n > 0) return `最长 ${n} 分钟`;
  }
  if (mode === 'unlimited') return '不限时';
  const seconds = defaults?.budget.run_timeout_seconds ?? 0;
  if (defaults?.limits?.task_timeout_limited === false || seconds <= 0) return '不限时';
  return `最长 ${Math.ceil(seconds / 60)} 分钟`;
}

export function unlimitedTimeoutPolicy(mode: DurationMode, minutes: string): { task_timeout_seconds: 0 } | undefined {
  return mode === 'unlimited' && !minutes.trim() ? { task_timeout_seconds: 0 } : undefined;
}

// Empty inputs mean inheritance. Never send zero as a pretend "unlimited" grant.
export function creationBudget(workers: string, minutes: string): CreationBudget {
  const budget: CreationBudget = {};
  if (workers.trim()) {
    const n = Number(workers);
    const error = validateWorkerCount(n);
    if (error) throw new Error(error);
    budget.max_parallel_workers = n;
  }
  if (minutes.trim()) {
    const n = Number(minutes);
    if (!Number.isSafeInteger(n) || n < 1 || n > 1440) throw new Error('运行时长须为 1 到 1440 分钟；留空继承服务器设置');
    budget.run_timeout_seconds = n * 60;
  }
  return budget;
}

export function creationName(name: string, objective: string): string {
  return name.trim() || Array.from(objective.trim().split('\n')[0]).slice(0, 60).join('');
}
