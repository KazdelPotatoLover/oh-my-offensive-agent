import { validateWorkerCount } from './budget';

export type CreationBudget = { max_parallel_workers?: number; run_timeout_seconds?: number };
export type CreationDefaults = {
  budget: { max_parallel_workers: number; run_timeout_seconds: number };
  model_configured: boolean;
};

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
