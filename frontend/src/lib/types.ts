export type RunStatus =
  | 'queued'
  | 'running'
  | 'stopping'
  | 'completed'
  | 'failed'
  | 'stopped'
  | 'interrupted';

export interface Task {
  id: string;
  name: string;
  objective: string;
  scope: Record<string, unknown>;
  config: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface Run {
  id: string;
  task_id: string;
  idempotency_key?: string;
  config_snapshot: Record<string, unknown>;
  status: RunStatus;
  finish_reason?: string;
  fail_at_step?: number;
  started_at?: string;
  finished_at?: string;
  created_at: string;
  updated_at: string;
}

export type RunEventType = 'state_change' | 'progress' | 'log';

export interface RunEvent {
  id: string;
  run_id: string;
  seq: number;
  type: RunEventType;
  payload: Record<string, unknown>;
  created_at: string;
}

export interface CreateTaskInput {
  name: string;
  objective: string;
  scope?: Record<string, unknown>;
  config?: Record<string, unknown>;
}

export interface UpdateTaskInput {
  name?: string;
  objective?: string;
  scope?: Record<string, unknown>;
  config?: Record<string, unknown>;
}

export interface CreateRunInput {
  fail_at_step?: number;
  idempotency_key?: string;
}

export interface TasksResponse {
  tasks: Task[];
}

export interface RunsResponse {
  runs: Run[];
}

export interface EventsResponse {
  events: RunEvent[];
}
