export type BudgetTemplate = {
  name: string;
  max_model_calls: number;
  max_tool_calls: number;
  run_timeout_seconds: number;
  max_parallel_workers: number;
  worker_max_model_calls: number;
  worker_max_tool_calls: number;
  worker_timeout_seconds: number;
  planner_reserved_model_calls: number;
  main_reserved_model_calls: number;
};

// Historical explicit presets retained for callers/tests. The creation form
// inherits managed defaults from the backend instead of submitting these.
export const EXPLORATION_TEMPLATE: BudgetTemplate = {
  name: 'exploration',
  max_model_calls: 80,
  max_tool_calls: 200,
  run_timeout_seconds: 600,
  max_parallel_workers: 3,
  worker_max_model_calls: 8,
  worker_max_tool_calls: 25,
  worker_timeout_seconds: 120,
  planner_reserved_model_calls: 16,
  main_reserved_model_calls: 8,
};

export const QUICK_TEMPLATE: BudgetTemplate = {
  name: 'quick_verification',
  max_model_calls: 30,
  max_tool_calls: 30,
  run_timeout_seconds: 120,
  max_parallel_workers: 1,
  worker_max_model_calls: 5,
  worker_max_tool_calls: 10,
  worker_timeout_seconds: 60,
  planner_reserved_model_calls: 6,
  main_reserved_model_calls: 4,
};

const ALIAS = /^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$/;

export function validateWorkerCount(value: number): string | null {
  if (!Number.isInteger(value) || value < 1 || value > 8) return 'Worker 池必须是 1 到 8 的整数';
  return null;
}

export function buildRunBudget(template: BudgetTemplate, workers: number): BudgetTemplate {
  const error = validateWorkerCount(workers);
  if (error) throw new Error(error);
  return { ...template, max_parallel_workers: workers };
}

export type RoleConfig = { runtime_profile: string; model: string };
export type AgentsConfig = { mainagent: RoleConfig; planner: RoleConfig; worker: RoleConfig; verifier: RoleConfig };

export function buildAgentsConfig(model: string, profile: string): AgentsConfig {
  if (!ALIAS.test(profile) || !ALIAS.test(model)) {
    throw new Error('runtime_profile 和 model 必须是服务器已配置的别名，不能是可执行代码或密钥');
  }
  const role = { runtime_profile: profile, model };
  return { mainagent: role, planner: role, worker: role, verifier: role };
}

export type ScopeResult =
  | { ok: true; scope: { mode: 'discovery'; seeds: string[]; discovery_policy: Record<string, unknown>; network_policy: Record<string, unknown> } }
  | { ok: false; error: string };

export function buildDiscoveryScope(input: { origin: string; seedPath: string }): ScopeResult {
  let parsed: URL;
  try {
    parsed = new URL(input.origin);
  } catch {
    return { ok: false, error: '范围 origin 必须是完整的 HTTP(S) URL' };
  }
  if (!['http:', 'https:'].includes(parsed.protocol) || !parsed.host || parsed.username || parsed.password) {
    return { ok: false, error: '范围 origin 必须是无凭据的 HTTP(S) URL' };
  }
  const seedPath = input.seedPath.trim() || `${parsed.pathname}${parsed.search}`;
  const path = seedPath.startsWith('/') ? seedPath : `/${seedPath}`;
  const origin = `${parsed.protocol}//${parsed.host}`;
  return {
    ok: true,
    scope: {
      mode: 'discovery',
      network_policy: {
        template: 'readonly', allowed_methods: ['GET', 'HEAD'],
        origins: [{ scheme: parsed.protocol.slice(0, -1), host: parsed.hostname.replace(/^\[|\]$/g, ''), port: parsed.port || (parsed.protocol === 'https:' ? '443' : '80'), path_mode: 'same_origin', allow_private: true }],
        max_pages: 20, max_requests: 100, request_interval_ms: 500,
      },
      seeds: [`${origin}${path}`],
      discovery_policy: {
        origin,
        mode: 'same_origin_html',
        allowed_methods: ['GET'],
        max_pages: 20,
        max_depth: 2,
        request_interval_ms: 500,
      },
    },
  };
}

export function executionGaps(taskConfig: unknown, executionInput: unknown): string[] {
  const config = taskConfig && typeof taskConfig === 'object' ? taskConfig as Record<string, unknown> : {};
  const input = executionInput && typeof executionInput === 'object' ? executionInput as Record<string, unknown> : {};
  const gaps: string[] = [];
  if (config.agents && !input.agents) gaps.push('execution_input.agents');
  if (config.budget && typeof config.budget === 'object') {
    gaps.push(...droppedBudgetFields(config.budget as Record<string, number>, { budget: input.budget }).map((key) => `budget.${key}`));
  }
  return gaps;
}

export function droppedBudgetFields(requested: Record<string, number>, executionInput: unknown): string[] {
  const budget = executionInput && typeof executionInput === 'object' && 'budget' in executionInput
    ? (executionInput as { budget?: unknown }).budget
    : undefined;
  const present = budget && typeof budget === 'object' ? budget as Record<string, unknown> : {};
  return Object.keys(requested).filter((key) => present[key] !== requested[key]);
}

export type TaskTimeoutOverride = { task_timeout_seconds: 0 };

export function buildTaskConfig(input: {
  provider: 'mock' | 'openai';
  profiles: unknown[];
  agents?: AgentsConfig;
  budget?: Partial<Omit<BudgetTemplate, 'name'>>;
  executionPolicy?: TaskTimeoutOverride;
  mockScenario?: string;
}): Record<string, unknown> {
  return {
    exploration: true,
    executor: 'orchestrator',
    provider: input.provider,
    ...(input.provider === 'mock' && input.mockScenario ? { mock_scenario: input.mockScenario } : {}),
    ...(input.profiles.length ? { profiles: input.profiles } : {}),
    ...(input.agents ? { agents: input.agents } : {}),
    ...(input.budget && Object.keys(input.budget).length ? { budget: input.budget } : {}),
    ...(input.executionPolicy ? { execution_policy: input.executionPolicy } : {}),
  };
}

export type LimitFlags = { known: boolean; model: boolean; tool: boolean; seconds: boolean };

export function limitFlags(budget: unknown): LimitFlags {
  const open = budget !== null && typeof budget === 'object' && !Array.isArray(budget);
  const limits = open ? (budget as { limits?: unknown }).limits : undefined;
  if (!limits || typeof limits !== 'object' || Array.isArray(limits)) {
    return { known: false, model: true, tool: true, seconds: true };
  }
  const row = limits as Record<string, unknown>;
  return {
    known: true,
    model: row.model_calls_limited !== false,
    tool: row.tool_calls_limited !== false,
    seconds: row.task_timeout_limited !== false,
  };
}

export function capText(limited: boolean, value: unknown): string {
  if (!limited) return '不设上限';
  return typeof value === 'number' && Number.isFinite(value) ? String(value) : '未知';
}

export function budgetSummaryText(budget: Record<string, unknown>): string {
  const flags = limitFlags(budget);
  const seconds = flags.seconds
    ? (typeof budget.run_timeout_seconds === 'number' ? `${budget.run_timeout_seconds} 秒` : '未知')
    : '不限时';
  return `模型 ${capText(flags.model, budget.max_model_calls)} · 工具 ${capText(flags.tool, budget.max_tool_calls)} · 时长 ${seconds}`;
}
