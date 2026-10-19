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
  execution_input?: Record<string, unknown>;
  status: RunStatus;
  finish_reason?: string;
  fail_at_step?: number;
  final_output?: string;
  started_at?: string;
  finished_at?: string;
  created_at: string;
  updated_at: string;
}

export type RunEventType =
  | 'state_change'
  | 'progress'
  | 'log'
  | 'model_call_start'
  | 'model_call_finish'
  | 'model_call_failed'
  | 'tool_call_start'
  | 'tool_call_success'
  | 'tool_call_failed'
  | 'tool_call_rejected'
  | 'final_answer';

export interface RunEvent {
  id: string;
  run_id: string;
  seq: number;
  type: RunEventType;
  payload: Record<string, unknown>;
  created_at: string;
}

export interface AgentMessage {
  id: string;
  run_id: string;
  seq: number;
  role: string;
  content: string;
  tool_call_id?: string;
  tool_calls?: unknown[];
  created_at: string;
}

export interface ModelCall {
  id: string;
  run_id: string;
  round: number;
  status: string;
  finish_reason?: string;
  request_id?: string;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  error_message?: string;
  started_at: string;
  finished_at?: string;
  created_at: string;
}

export interface AgentToolCall {
  id: string;
  run_id: string;
  model_call_id?: string;
  call_id: string;
  tool_name: string;
  arguments: Record<string, unknown>;
  output: string;
  status: string;
  error_message?: string;
  started_at?: string;
  finished_at?: string;
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
  executor?: 'fake' | 'agent' | 'orchestrator';
  provider?: 'mock' | 'openai';
  mock_scenario?: string;
  idempotency_key?: string;
  budget?: {
    max_model_calls?: number;
    max_tool_calls?: number;
    max_parallel_workers?: number;
    run_timeout_seconds?: number;
  };
}

export type WorkItemStatus =
  | 'pending'
  | 'running'
  | 'completed'
  | 'failed'
  | 'cancelled'
  | 'blocked';

export interface WorkItem {
  id: string;
  run_id: string;
  key: string;
  title: string;
  objective: string;
  work_type: string;
  status: WorkItemStatus;
  blocked_reason?: string;
  input_data: Record<string, unknown>;
  output_data: Record<string, unknown>;
  current_attempt_id?: string;
  created_at: string;
  updated_at: string;
}

export interface Observation {
  id: string;
  run_id: string;
  attempt_id: string;
  obs_type: string;
  content: string;
  data: Record<string, unknown>;
  source_tool_call_id: string;
  created_at: string;
}

export interface AgentSession {
  id: string;
  run_id: string;
  role: 'planner' | 'worker' | 'legacy_agent';
  round?: number;
  work_item_id?: string;
  attempt_id?: string;
  status: string;
  input_snapshot: Record<string, unknown>;
  finish_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface PlannerRound {
  id: string;
  run_id: string;
  round: number;
  trigger_reason: string;
  session_id?: string;
  plan_summary?: string;
  status: string;
  created_at: string;
  finished_at?: string;
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

export interface MessagesResponse {
  messages: AgentMessage[];
}

export interface ModelCallsResponse {
  model_calls: ModelCall[];
}

export interface ToolCallsResponse {
  tool_calls: AgentToolCall[];
}

export interface WorkItemsResponse {
  work_items: WorkItem[];
}

export interface ObservationsResponse {
  observations: Observation[];
}

export interface SessionsResponse {
  sessions: AgentSession[];
}

export interface PlannerRoundsResponse {
  planner_rounds: PlannerRound[];
}

