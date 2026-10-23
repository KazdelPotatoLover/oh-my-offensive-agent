export type RunStatus =
  | 'queued'
  | 'running'
  | 'stopping'
  | 'completed'
  | 'failed'
  | 'stopped'
  | 'interrupted'
  | 'paused';

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
  current_epoch: number;
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
  response_id?: string;
  usage_known?: boolean;
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
    worker_max_model_calls?: number;
    worker_max_tool_calls?: number;
    worker_timeout_seconds?: number;
    planner_reserved_model_calls?: number;
    main_reserved_model_calls?: number;
    run_timeout_seconds?: number;
    model_timeout_seconds?: number;
    tool_timeout_seconds?: number;
    summary_timeout_seconds?: number;
    cleanup_timeout_seconds?: number;
  };
}

export type WorkItemStatus =
  | 'pending'
  | 'running'
  | 'completed'
  | 'failed'
  | 'exhausted'
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
  created_by_planner_round_id?: string;
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
  asset_id?: string;
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

// --- Phase 4: Asset and Traceability Models ---

export interface Asset {
  id: string;
  namespace: string;
  kind: string;
  external_key: string;
  name: string;
  description: string;
  metadata: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface AssetSummary extends Asset {
  run_count: number;
  observation_count: number;
  work_item_count: number;
}

export interface ObservationDetail extends Observation {
  tool_call?: AgentToolCall;
}

export interface WorkItemDetail extends WorkItem {
  target_assets: Asset[];
  source_observations: Observation[];
  dependencies: WorkItem[];
  created_round_num?: number;
}

export interface AssetRunHistory {
  run_id: string;
  task_id: string;
  task_name: string;
  run_status: RunStatus;
  finish_reason?: string;
  started_at?: string;
  finished_at?: string;
  work_items: WorkItemDetail[];
  observations: ObservationDetail[];
  discovered_by?: AssetDiscoveryDetail;
}

export interface AssetDiscoveryDetail {
  id: string;
  run_id: string;
  source_asset_id: string;
  source_tool_call_id: string;
  target_asset_id: string;
  raw_href: string;
  discovered_at: string;
  source_asset_name?: string;
  source_asset_key?: string;
  target_asset_name?: string;
  target_asset_key?: string;
}

export interface DiscoveriesResponse {
  discoveries: AssetDiscoveryDetail[];
}

export interface AssetDetailsResponse {
  asset: Asset;
  run_history: AssetRunHistory[];
}

export interface AssetsResponse {
  assets: AssetSummary[];
}

export interface RunAssetsResponse {
  assets: Asset[];
}

export interface WorkItemDetailsResponse {
  work_items: WorkItemDetail[];
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

export type CheckStatus = 'pass' | 'needs_attention' | 'not_applicable' | 'insufficient_evidence' | 'error';
export interface CheckResult {
  id: string;
  run_id: string;
  asset_id: string;
  check_id: string;
  check_name: string;
  check_version: string;
  status: CheckStatus;
  explanation: string;
  source_tool_call_id?: string;
  evidence_snapshot: Record<string, unknown>;
}
export interface Finding {
  id: string;
  run_id: string;
  asset_id: string;
  check_result_id: string;
  title: string;
  category: string;
  severity: string;
  status: 'pending_review' | 'confirmed' | 'false_positive' | 'ignored';
  description: string;
  remediation: string;
  source_tool_call_id?: string;
  evidence_ref?: Record<string, unknown>;
  created_at: string;
  review_version: number;
  last_reviewed_at?: string;
  last_reviewer?: string;
  last_review_comment?: string;
  source_kind?: 'check_rule' | 'speculative' | 'empirical';
  verification_state?: 'unverified' | 'verified' | 'retested';
  retest_verdict?: string;
  retest_notes?: string;
  retest_at?: string;
}

export interface Goal extends ExplorationNode {
  kind: 'goal';
  state: 'open' | 'met' | 'blocked' | 'abandoned';
  completed_at?: string;
}

export interface ExplorationNode {
  id: string;
  run_id: string;
  kind: 'begin' | 'goal' | 'intent' | 'fact' | 'finding' | 'hint' | 'digest';
  payload: Record<string, any>;
  priority: number;
  state: string;
  origin?: string;
  owner?: string;
  work_item_id?: string;
  finding_id?: string;
  epoch_number: number;
  created_at: string;
}

export interface ExecutionEpoch {
  id: string;
  run_id: string;
  epoch_number: number;
  status: string;
  started_at: string;
  finished_at?: string;
  finish_reason?: string;
  summary?: string;
  created_at: string;
}

export interface BudgetGrant {
  id: string;
  run_id: string;
  epoch_number: number;
  granted_by: string;
  additional_model_calls: number;
  additional_tool_calls: number;
  additional_seconds: number;
  reason: string;
  idempotency_key?: string;
  created_at: string;
}

export interface FindingRetest {
  id: string;
  finding_id: string;
  run_id: string;
  status: string;
  verdict?: 'still_reproducible' | 'fixed' | 'inconclusive';
  notes: string;
  evidence: string;
  summary: string;
  session_id?: string;
  created_at: string;
  finished_at?: string;
}

export interface FindingReview {
  id: string;
  finding_id: string;
  run_id: string;
  from_status: 'pending_review' | 'confirmed' | 'false_positive' | 'ignored';
  to_status: 'pending_review' | 'confirmed' | 'false_positive' | 'ignored';
  from_version: number;
  to_version: number;
  reviewer: string;
  comment: string;
  request_id: string;
  created_at: string;
}

export interface FindingReportSummary {
  total: number;
  pending_review: number;
  confirmed: number;
  false_positive: number;
  ignored: number;
}

export interface EffectiveBudget {
  context_window_bytes?: number;
  max_model_calls: number;
  max_tool_calls: number;
  run_timeout_seconds: number;
  model_timeout_seconds?: number;
  tool_timeout_seconds?: number;
  max_planner_rounds?: number;
  max_work_items?: number;
  worker_max_model_calls?: number;
  worker_max_tool_calls?: number;
  worker_timeout_seconds?: number;
  epoch_number?: number;
}

export type BudgetConfig = EffectiveBudget;

export interface FindingReportItem {
  finding: Finding;
  check_result: {
    id: string;
    check_id: string;
    check_name: string;
    check_version: string;
    status: CheckStatus;
    explanation: string;
    source_tool_call_id?: string;
    evidence_snapshot?: Record<string, unknown>;
  };
}

export interface FindingReport {
  run_id: string;
  generated_at: string;
  summary: FindingReportSummary;
  findings: FindingReportItem[];
}

export interface MCPTool {
  id: number;
  server_id: number;
  tool_name: string;
  description: string;
  schema?: Record<string, unknown>;
  discovered_at: string;
}

export interface MCPServer {
  id: number;
  name: string;
  transport: 'stdio' | 'http' | 'sse';
  command?: string;
  args?: string[];
  env?: Record<string, string>;
  url?: string;
  enabled: boolean;
  insecure: boolean;
  created_at: string;
  updated_at: string;
  tools?: MCPTool[];
}
