import { parseExplorationGraph, type ExplorationGraph } from './workbench/graph';
import type { CreationDefaults } from './workbench/creation';
import {
  CheckResult,
  Finding,
  FindingReview,
  FindingReport,
  Task,
  Run,
  RunEvent,
  AgentMessage,
  ModelCall,
  AgentToolCall,
  WorkItem,
  Observation,
  AgentSession,
  PlannerRound,
  Asset,
  AssetSummary,
  WorkItemDetail,
  AssetDetailsResponse,
  AssetsResponse,
  RunAssetsResponse,
  WorkItemDetailsResponse,
  AssetDiscoveryDetail,
  DiscoveriesResponse,
  CreateTaskInput,
  UpdateTaskInput,
  CreateRunInput,
  TasksResponse,
  RunsResponse,
  EventsResponse,
  MessagesResponse,
  ModelCallsResponse,
  ToolCallsResponse,
  WorkItemsResponse,
  ObservationsResponse,
  SessionsResponse,
  PlannerRoundsResponse,
  Goal,
  ExplorationNode,
  BudgetGrant,
  ExecutionEpoch,
  FindingRetest,
  BudgetConfig,
  MCPServer,
  NativeToolItem,
} from './types';

const API_BASE_URL =
  typeof window !== 'undefined'
    ? ''
    : process.env.INTERNAL_API_URL ||
      process.env.NEXT_PUBLIC_API_URL ||
      'http://127.0.0.1:28080';

// getRandomValues also works on a LAN HTTP origin, where randomUUID is absent.
export function newRequestID(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(16));
  bytes[6] = (bytes[6] & 15) | 64;
  bytes[8] = (bytes[8] & 63) | 128;
  const hex = Array.from(bytes, value => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}

export class ApiError extends Error {
  status: number;
  data?: unknown;
  code?: string;
  scope?: string;
  retryable?: boolean;
  suggestedAction?: string;

  constructor(message: string, status: number, data?: unknown, extra?: { code?: string; scope?: string; retryable?: boolean; suggestedAction?: string }) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.data = data;
    this.code = extra?.code;
    this.scope = extra?.scope;
    this.retryable = extra?.retryable;
    this.suggestedAction = extra?.suggestedAction;
  }
}

async function request<T>(
  endpoint: string,
  options: RequestInit = {}
): Promise<T> {
  const url = `${API_BASE_URL}${endpoint}`;
  const headers = new Headers(options.headers || {});

  if (!headers.has('Content-Type') && options.body && typeof options.body === 'string') {
    headers.set('Content-Type', 'application/json');
  }

  const response = await fetch(url, {
    ...options,
    headers,
  });

  if (!response.ok) {
    let errorMessage = `HTTP error ${response.status}: ${response.statusText}`;
    let errorData: unknown;
    try {
      const text = await response.text();
      if (text) {
        try {
          const json = JSON.parse(text);
          errorData = json;
          if (json.error && typeof json.error === 'string') {
            errorMessage = json.error;
          } else if (json.error && typeof json.error === 'object' && typeof json.error.message === 'string') {
            errorMessage = json.error.code ? `${json.error.code}: ${json.error.message}` : json.error.message;
          } else if (json.message && typeof json.message === 'string') {
            errorMessage = json.message;
          } else {
            errorMessage = text;
          }
        } catch {
          errorMessage = text;
        }
      }
    } catch {
      // Ignore body read errors and use standard error message
    }
    const envelope = errorData && typeof errorData === 'object' && 'error' in errorData ? (errorData as { error?: { code?: string; scope?: string; retryable?: boolean; suggested_action?: string } }).error : undefined;
    throw new ApiError(errorMessage, response.status, errorData, envelope && typeof envelope === 'object' ? {
      code: envelope.code,
      scope: envelope.scope,
      retryable: envelope.retryable,
      suggestedAction: envelope.suggested_action,
    } : undefined);
  }

  if (response.status === 204) {
    return undefined as unknown as T;
  }

  const text = await response.text();
  if (!text) {
    return undefined as unknown as T;
  }

  return JSON.parse(text) as T;
}

export const api = {
  async getCreationDefaults(): Promise<CreationDefaults> {
    return request<CreationDefaults>('/api/runtime/defaults');
  },
  async getAssetRelations(assetId: string): Promise<any[]> { const data = await request<{relations:any[]}>(`/api/assets/${assetId}/relations`); return data.relations || []; },
  async getCapabilities(runId: string): Promise<{profiles: Record<string, unknown>[]}> { return request(`/api/runs/${runId}/capabilities`); },
  async getArtifacts(runId: string, offset = 0): Promise<{artifacts: any[];has_more:boolean}> { return request(`/api/runs/${runId}/artifacts?offset=${offset}&limit=25`); },
  async readArtifact(runId: string, artifactId: string, offset = 0): Promise<any> { return request(`/api/runs/${runId}/artifacts/${artifactId}?offset=${offset}&limit=8192`); },
  async getCheckpoints(runId: string): Promise<{checkpoints: Record<string, unknown>[]}> { return request(`/api/runs/${runId}/checkpoints`); },
  async getGraphPage(runId: string, offset = 0): Promise<any> { return request(`/api/runs/${runId}/graph/overview?offset=${offset}&limit=25`); },
  async getNodeDetail(runId: string, nodeId: string): Promise<any> { return request(`/api/runs/${runId}/graph/nodes/${nodeId}`); },
  async reviseNode(runId: string, nodeId: string, input: {expected_version:number;actor:string;reason:string;state:string;payload?:Record<string,unknown>}): Promise<void> { await request(`/api/runs/${runId}/graph/nodes/${nodeId}/revisions`, {method:'POST',body:JSON.stringify(input)}); },
  // Tasks
  async getTasks(): Promise<Task[]> {
    const data = await request<TasksResponse>('/api/tasks');
    return data.tasks || [];
  },

  async createTask(input: CreateTaskInput): Promise<Task> {
    return request<Task>('/api/tasks', {
      method: 'POST',
      body: JSON.stringify({
        name: input.name,
        objective: input.objective,
        scope: input.scope || {},
        config: input.config || {},
      }),
    });
  },

  async getTask(id: string): Promise<Task> {
    return request<Task>(`/api/tasks/${id}`);
  },

  async updateTask(id: string, input: UpdateTaskInput): Promise<Task> {
    return request<Task>(`/api/tasks/${id}`, {
      method: 'PUT',
      body: JSON.stringify(input),
    });
  },

  async deleteTask(id: string): Promise<void> {
    return request<void>(`/api/tasks/${id}`, {
      method: 'DELETE',
    });
  },

  // Runs
  async getRuns(taskId: string): Promise<Run[]> {
    const data = await request<RunsResponse>(`/api/tasks/${taskId}/runs`);
    return data.runs || [];
  },

  async createRun(taskId: string, input?: CreateRunInput): Promise<Run> {
    const headers: Record<string, string> = {
      'Content-Type': 'application/json',
    };
    if (input?.idempotency_key) {
      headers['Idempotency-Key'] = input.idempotency_key;
    }

    const body: Record<string, unknown> = {};
    if (input?.fail_at_step !== undefined) {
      body.fail_at_step = input.fail_at_step;
    }
    if (input?.executor !== undefined) {
      body.executor = input.executor;
    }
    if (input?.provider !== undefined) {
      body.provider = input.provider;
    }
    if (input?.mock_scenario !== undefined) {
      body.mock_scenario = input.mock_scenario;
    }
    if (input?.budget !== undefined) {
      body.budget = input.budget;
    }
    if (input?.execution_policy !== undefined) {
      body.execution_policy = input.execution_policy;
    }

    return request<Run>(`/api/tasks/${taskId}/runs`, {
      method: 'POST',
      headers,
      body: JSON.stringify(body),
    });
  },

  async getRun(id: string): Promise<Run> {
    return request<Run>(`/api/runs/${id}`);
  },

  async stopRun(id: string): Promise<Run> {
    return request<Run>(`/api/runs/${id}/stop`, {
      method: 'POST',
    });
  },

  // Events
  async getRunEvents(runId: string): Promise<RunEvent[]> {
    const data = await request<EventsResponse>(`/api/runs/${runId}/events`);
    return data.events || [];
  },

  // Agent Runtime Data
  async getRunMessages(runId: string): Promise<AgentMessage[]> {
    const data = await request<MessagesResponse>(`/api/runs/${runId}/messages`);
    return data.messages || [];
  },

  async getRunModelCalls(runId: string): Promise<ModelCall[]> {
    const data = await request<ModelCallsResponse>(`/api/runs/${runId}/model_calls`);
    return data.model_calls || [];
  },

  async getRunToolCalls(runId: string): Promise<AgentToolCall[]> {
    const data = await request<ToolCallsResponse>(`/api/runs/${runId}/tool_calls`);
    return data.tool_calls || [];
  },

  // Orchestration & Multi-Worker Data
  async getRunWorkItems(runId: string): Promise<WorkItem[]> {
    const data = await request<WorkItemsResponse>(`/api/runs/${runId}/work_items`);
    return data.work_items || [];
  },

  async getRunChecks(runId: string): Promise<CheckResult[]> {
    const data = await request<{ check_results: CheckResult[] }>(`/api/runs/${runId}/checks`);
    return data.check_results || [];
  },

  async getRunFindings(runId: string): Promise<Finding[]> {
    const data = await request<{ findings: Finding[] }>(`/api/runs/${runId}/findings`);
    return data.findings || [];
  },

  async getFindingReviews(runId: string, findingId: string): Promise<FindingReview[]> {
    const data = await request<{ reviews: FindingReview[] }>(`/api/runs/${runId}/findings/${findingId}/reviews`);
    return data.reviews || [];
  },

  async reviewFinding(
    runId: string,
    findingId: string,
    input: {
      status: 'pending_review' | 'confirmed' | 'false_positive' | 'ignored';
      comment: string;
      reviewer: string;
      expected_version: number;
      request_id: string;
    }
  ): Promise<{ finding: Finding; review: FindingReview; replayed: boolean }> {
    return request<{ finding: Finding; review: FindingReview; replayed: boolean }>(
      `/api/runs/${runId}/findings/${findingId}/reviews`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(input),
      }
    );
  },

  async getFindingReport(runId: string): Promise<FindingReport> {
    return request<FindingReport>(`/api/runs/${runId}/finding-report`);
  },

  async getRunObservations(runId: string): Promise<Observation[]> {
    const data = await request<ObservationsResponse>(`/api/runs/${runId}/observations`);
    return data.observations || [];
  },

  async getRunSessions(runId: string): Promise<AgentSession[]> {
    const data = await request<SessionsResponse>(`/api/runs/${runId}/sessions`);
    return data.sessions || [];
  },

  async getRunPlannerRounds(runId: string): Promise<PlannerRound[]> {
    const data = await request<PlannerRoundsResponse>(`/api/runs/${runId}/planner_rounds`);
    return data.planner_rounds || [];
  },

  // Phase 4: Assets & Traceability
  async getAssets(): Promise<AssetSummary[]> {
    const data = await request<AssetsResponse>('/api/assets');
    return data.assets || [];
  },

  async getAsset(assetId: string): Promise<Asset> {
    return request<Asset>(`/api/assets/${assetId}`);
  },

  async getAssetDetails(assetId: string): Promise<AssetDetailsResponse> {
    return request<AssetDetailsResponse>(`/api/assets/${assetId}/details`);
  },

  async getRunAssets(runId: string): Promise<Asset[]> {
    const data = await request<RunAssetsResponse>(`/api/runs/${runId}/assets`);
    return data.assets || [];
  },

  async getRunWorkItemDetails(runId: string): Promise<WorkItemDetail[]> {
    const data = await request<WorkItemDetailsResponse>(`/api/runs/${runId}/work_items/details`);
    return data.work_items || [];
  },

  async getRunDiscoveries(runId: string): Promise<AssetDiscoveryDetail[]> {
    const data = await request<DiscoveriesResponse>(`/api/runs/${runId}/discoveries`);
    return data.discoveries || [];
  },

  // ARTEX Alignment: Dual Graph & Goals
  async getGoals(runId: string): Promise<Goal[]> {
    const data = await request<{ goals: Goal[] }>(`/api/runs/${runId}/goals`);
    return data.goals || [];
  },

  async addHint(runId: string, text: string, assetIDs: string[]): Promise<void> {
    await request(`/api/runs/${runId}/hints`, { method: 'POST', body: JSON.stringify({ text, asset_ids: assetIDs }) });
  },

  async proveGoal(runId: string, goalId: string, evidenceID: string, reason: string): Promise<{ all_goals_met: boolean }> {
    return request<{ all_goals_met: boolean }>(`/api/runs/${runId}/goals/${goalId}/prove`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ evidence_id: evidenceID, reason }),
    });
  },

  async getExplorationGraph(runId: string): Promise<ExplorationGraph> {
    return parseExplorationGraph(await request<unknown>(`/api/runs/${runId}/graph`));
  },

  async getGraphOverview(runId: string): Promise<{ nodes: ExplorationNode[]; total_nodes: number }> {
    return request<{ nodes: ExplorationNode[]; total_nodes: number }>(`/api/runs/${runId}/graph/overview`);
  },

  // Execution Control & Budget Grants
  async pauseRun(runId: string): Promise<void> {
    await request(`/api/runs/${runId}/pause`, { method: 'POST' });
  },

  async resumeRun(runId: string): Promise<void> {
    await request(`/api/runs/${runId}/resume`, { method: 'POST' });
  },

  async steerWorkItem(runId: string, workItemId: string, instructions: string, inputDataOverride?: any, idempotencyKey?: string): Promise<void> {
    await request(`/api/runs/${runId}/work_items/${workItemId}/steer`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...(idempotencyKey ? { 'Idempotency-Key': idempotencyKey } : {}) },
      body: JSON.stringify({ instructions, input_data_override: inputDataOverride }),
    });
  },

  async killWorkItem(runId: string, workItemId: string, reason?: string): Promise<void> {
    await request(`/api/runs/${runId}/work_items/${workItemId}/kill`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ reason: reason || 'killed_by_user' }),
    });
  },

  async grantBudget(runId: string, grant: { additional_model_calls?: number; additional_tool_calls?: number; additional_seconds?: number; reason: string; idempotency_key?: string }): Promise<BudgetGrant> {
    return request<BudgetGrant>(`/api/runs/${runId}/grant_budget`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(grant),
    });
  },

  async getEffectiveBudget(runId: string): Promise<BudgetConfig> {
    return request<BudgetConfig>(`/api/runs/${runId}/budget/effective`);
  },

  async listBudgetGrants(runId: string): Promise<BudgetGrant[]> {
    const data = await request<{ grants: BudgetGrant[] }>(`/api/runs/${runId}/budget_grants`);
    return data.grants || [];
  },

  async listEpochs(runId: string): Promise<ExecutionEpoch[]> {
    const data = await request<{ epochs: ExecutionEpoch[] }>(`/api/runs/${runId}/epochs`);
    return data.epochs || [];
  },

  async startNewEpoch(runId: string, reason: string, summary: string, expectedEpoch?: number): Promise<{ epoch_number: number }> {
    return request<{ epoch_number: number }>(`/api/runs/${runId}/epochs/new`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', 'Idempotency-Key': expectedEpoch ? `${runId}-epoch-${expectedEpoch}` : newRequestID() },
      body: JSON.stringify({ reason, summary, expected_epoch: expectedEpoch }),
    });
  },

  // Retests & Findings
  async listFindingRetests(runId: string, findingId: string): Promise<FindingRetest[]> {
    const data = await request<{ retests: FindingRetest[] }>(`/api/runs/${runId}/findings/${findingId}/retests`);
    return data.retests || [];
  },

  async createFindingRetest(runId: string, findingId: string, notes: string): Promise<{ retest: FindingRetest; session: AgentSession }> {
    return request<{ retest: FindingRetest; session: AgentSession }>(`/api/runs/${runId}/findings/${findingId}/retests`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ notes }),
    });
  },

  async finishFindingRetest(runId: string, findingId: string, retestId: string, verdict: string, evidence: string, summary: string): Promise<FindingRetest> {
    return request<FindingRetest>(`/api/runs/${runId}/findings/${findingId}/retests/${retestId}/finish`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ verdict, evidence, summary }),
    });
  },

  // MCP Servers
  async listMCPServers(): Promise<{ servers: MCPServer[] }> {
    return request<{ servers: MCPServer[] }>(`/api/mcp/servers`);
  },
  async getMCPServer(id: number): Promise<MCPServer> {
    return request<MCPServer>(`/api/mcp/servers/${id}`);
  },
  async createMCPServer(input: Partial<MCPServer>): Promise<MCPServer> {
    return request<MCPServer>(`/api/mcp/servers`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    });
  },
  async updateMCPServer(id: number, input: Partial<MCPServer>): Promise<MCPServer> {
    return request<MCPServer>(`/api/mcp/servers/${id}`, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(input),
    });
  },
  async deleteMCPServer(id: number): Promise<{ status: string }> {
    return request<{ status: string }>(`/api/mcp/servers/${id}`, {
      method: 'DELETE',
    });
  },
  async discoverMCPTools(id: number): Promise<{ tools: any[] }> {
    return request<{ tools: any[] }>(`/api/mcp/servers/${id}/discover`, {
      method: 'POST',
    });
  },

  // Native Tools Catalog
  async getNativeToolsCatalog(): Promise<{ tools: NativeToolItem[] }> {
    return request<{ tools: NativeToolItem[] }>(`/api/tools/catalog`);
  },
};
