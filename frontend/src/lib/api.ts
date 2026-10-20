import {
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
} from './types';

const API_BASE_URL =
  typeof window !== 'undefined'
    ? ''
    : process.env.INTERNAL_API_URL ||
      process.env.NEXT_PUBLIC_API_URL ||
      'http://127.0.0.1:28080';


export class ApiError extends Error {
  status: number;
  data?: unknown;

  constructor(message: string, status: number, data?: unknown) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.data = data;
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
    throw new ApiError(errorMessage, response.status, errorData);
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
};
