export class ContractParseError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'ContractParseError';
  }
}

export type CommandType =
  | 'hint'
  | 'add_intent'
  | 'steer_work'
  | 'kill_work'
  | 'continue_work'
  | 'retry_work'
  | 'pause_run'
  | 'resume_run'
  | 'stop_run'
  | 'start_epoch'
  | 'grant_budget';

export type WorkbenchErrorBody = {
  code: string;
  scope: string;
  message: string;
  retryable: boolean;
  source_id?: string;
  suggested_action?: string;
};

export type WorkerCapacity = { requested: number; effective: number; active: number; queued: number };
export type ModelCapacity = { limit: number; in_flight: number; queued: number };

export type DirectionView = {
  work_item_id: string;
  objective: string;
  source_refs?: string[];
  status: string;
  attempt_id?: string;
  session_id?: string;
  latest_step?: string;
  progress_at?: string;
  local_budget?: unknown;
  error?: WorkbenchErrorBody;
  allowed_actions?: CommandType[];
};

export type WorkbenchSnapshot = {
  schema_version: 'workbench.v1';
  snapshot_at: string;
  run: { id: string; status: string; epoch_number: number };
  worker_capacity: WorkerCapacity;
  model_capacity: ModelCapacity;
  origin_pacing?: { waiting: boolean; reason?: string };
  budget: Record<string, unknown>;
  goals: unknown[];
  planner: { status: string; reason?: string; session_id?: string };
  directions: DirectionView[];
  main_conversation: { last_seq: number; active_turn_id: string | null };
  events: { seq: number; type: string; payload?: unknown }[];
  last_event_seq: number;
  has_more_events: boolean;
};

export type MainMessage = {
  seq: number;
  id: string;
  role: 'user' | 'assistant' | 'action';
  text?: string;
  turn_id?: string;
  turn_status?: string;
  command_id?: string;
  model_call_id?: string;
  tool_call_id?: string;
  work_item_id?: string;
};

export type MainMessagePage = { messages: MainMessage[]; last_seq: number; has_more: boolean };

export type CommandBody = {
  type: CommandType;
  expected_epoch: number;
  work_item_id?: string;
  expected_attempt_id?: string;
  payload?: Record<string, unknown>;
};

export type CommandResult = {
  command_id: string;
  status: 'accepted' | 'applied' | 'rejected';
  type: CommandType;
  epoch_number: number;
  work_item_id?: string;
  attempt_id?: string;
  delivered: boolean;
  result?: unknown;
  error?: WorkbenchErrorBody;
};

const WORK_ITEM_ACTIONS = new Set<CommandType>(['steer_work', 'kill_work', 'continue_work', 'retry_work']);
const PLANNER_STATUS = new Set(['idle', 'planning', 'waiting', 'needs_input', 'failed', 'stopped']);
const TURN_STATUS = new Set(['queued', 'running', 'completed', 'failed', 'cancelled']);

function isObject(value: unknown): value is Record<string, unknown> {
  return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
}

function requiredString(source: Record<string, unknown>, key: string, label: string): string {
  const value = source[key];
  if (typeof value !== 'string' || value.trim() === '') throw new ContractParseError(`${label} requires ${key}`);
  return value;
}

function requiredNumber(source: Record<string, unknown>, key: string, label: string): number {
  const value = source[key];
  if (typeof value !== 'number' || !Number.isFinite(value)) throw new ContractParseError(`${label} requires ${key}`);
  return value;
}

function optionalUuid(value: unknown, label: string): string | undefined {
  if (value == null) return undefined;
  if (typeof value !== 'string' || value.trim() === '') throw new ContractParseError(`${label} must be a string`);
  return value;
}

export function parseEnvelope(input: unknown): WorkbenchErrorBody | null {
  if (!isObject(input) || !isObject(input.error)) return null;
  const error = input.error;
  if (typeof error.code !== 'string' || typeof error.message !== 'string') return null;
  return {
    code: error.code,
    scope: typeof error.scope === 'string' ? error.scope : 'request',
    message: error.message,
    retryable: error.retryable === true,
    source_id: typeof error.source_id === 'string' ? error.source_id : undefined,
    suggested_action: typeof error.suggested_action === 'string' ? error.suggested_action : undefined,
  };
}

function parseError(value: unknown, label: string): WorkbenchErrorBody | undefined {
  if (value == null) return undefined;
  const envelope = parseEnvelope({ error: value });
  if (!envelope) throw new ContractParseError(`${label} error is incomplete`);
  return envelope;
}

export function parseWorkbenchSnapshot(input: unknown): WorkbenchSnapshot {
  if (!isObject(input)) throw new ContractParseError('workbench snapshot must be an object');
  if (input.schema_version !== 'workbench.v1') throw new ContractParseError('schema_version must be workbench.v1');
  if (typeof input.snapshot_at !== 'string' || input.snapshot_at === '') throw new ContractParseError('snapshot_at is required');
  if (!isObject(input.run)) throw new ContractParseError('run is required');
  const epoch = requiredNumber(input.run, 'epoch_number', 'run');
  if (epoch < 1) throw new ContractParseError('run.epoch_number must be >= 1');
  if (!isObject(input.worker_capacity) || !isObject(input.model_capacity)) {
    throw new ContractParseError('worker_capacity and model_capacity are required');
  }
  const worker = {
    requested: requiredNumber(input.worker_capacity, 'requested', 'worker_capacity'),
    effective: requiredNumber(input.worker_capacity, 'effective', 'worker_capacity'),
    active: requiredNumber(input.worker_capacity, 'active', 'worker_capacity'),
    queued: requiredNumber(input.worker_capacity, 'queued', 'worker_capacity'),
  };
  const model = {
    limit: requiredNumber(input.model_capacity, 'limit', 'model_capacity'),
    in_flight: requiredNumber(input.model_capacity, 'in_flight', 'model_capacity'),
    queued: requiredNumber(input.model_capacity, 'queued', 'model_capacity'),
  };
  for (const value of [...Object.values(worker), ...Object.values(model)]) {
    if (value < 0) throw new ContractParseError('capacity counts must be non-negative');
  }
  if (!isObject(input.budget)) throw new ContractParseError('budget must be present, use an empty object when unknown');
  if (!Array.isArray(input.goals)) throw new ContractParseError('goals must be an array');
  if (!Array.isArray(input.directions)) throw new ContractParseError('directions must be an array');
  if (!Array.isArray(input.events)) throw new ContractParseError('events must be an array');
  if (!isObject(input.planner) || !PLANNER_STATUS.has(String(input.planner.status))) {
    throw new ContractParseError('unknown planner status');
  }
  if (!isObject(input.main_conversation)) throw new ContractParseError('main_conversation is required');
  const lastSeq = requiredNumber(input.main_conversation, 'last_seq', 'main_conversation');
  const lastEventSeq = requiredNumber(input, 'last_event_seq', 'snapshot');
  if (lastSeq < 0 || lastEventSeq < 0) throw new ContractParseError('cursors must be non-negative');
  if (typeof input.has_more_events !== 'boolean') throw new ContractParseError('has_more_events must be boolean');
  return {
    schema_version: 'workbench.v1',
    snapshot_at: input.snapshot_at,
    run: { id: requiredString(input.run, 'id', 'run'), status: requiredString(input.run, 'status', 'run'), epoch_number: epoch },
    worker_capacity: worker,
    model_capacity: model,
    origin_pacing: isObject(input.origin_pacing)
      ? { waiting: input.origin_pacing.waiting === true, reason: typeof input.origin_pacing.reason === 'string' ? input.origin_pacing.reason : undefined }
      : undefined,
    budget: input.budget,
    goals: input.goals,
    planner: {
      status: String(input.planner.status),
      reason: typeof input.planner.reason === 'string' ? input.planner.reason : undefined,
      session_id: optionalUuid(input.planner.session_id, 'planner.session_id'),
    },
    directions: input.directions.map((item, index) => parseDirection(item, index)),
    main_conversation: {
      last_seq: lastSeq,
      active_turn_id: input.main_conversation.active_turn_id == null ? null : requiredString(input.main_conversation, 'active_turn_id', 'main_conversation'),
    },
    events: input.events.map((event, index) => {
      if (!isObject(event) || typeof event.seq !== 'number' || typeof event.type !== 'string') {
        throw new ContractParseError(`events[${index}] requires seq and type`);
      }
      return { seq: event.seq, type: event.type, payload: event.payload };
    }),
    last_event_seq: lastEventSeq,
    has_more_events: input.has_more_events,
  };
}

function parseDirection(input: unknown, index: number): DirectionView {
  if (!isObject(input)) throw new ContractParseError(`directions[${index}] must be an object`);
  const actions = input.allowed_actions;
  let allowed: CommandType[] | undefined;
  if (actions != null) {
    if (!Array.isArray(actions)) throw new ContractParseError('allowed_actions must be an array');
    allowed = actions.map((action) => {
      if (!WORK_ITEM_ACTIONS.has(action as CommandType)) throw new ContractParseError('direction allowed_actions must be work-item commands');
      return action as CommandType;
    });
  }
  return {
    work_item_id: requiredString(input, 'work_item_id', 'direction'),
    objective: typeof input.objective === 'string' ? input.objective : '',
    source_refs: Array.isArray(input.source_refs) ? input.source_refs.filter((item): item is string => typeof item === 'string') : undefined,
    status: requiredString(input, 'status', 'direction'),
    attempt_id: optionalUuid(input.attempt_id, 'attempt_id'),
    session_id: optionalUuid(input.session_id, 'session_id'),
    latest_step: typeof input.latest_step === 'string' ? input.latest_step : undefined,
    progress_at: typeof input.progress_at === 'string' ? input.progress_at : undefined,
    local_budget: input.local_budget,
    error: parseError(input.error, 'direction'),
    allowed_actions: allowed,
  };
}

export function parseMainMessagePage(input: unknown): MainMessagePage {
  if (!isObject(input) || !Array.isArray(input.messages)) throw new ContractParseError('messages must be an array');
  if (typeof input.last_seq !== 'number' || typeof input.has_more !== 'boolean') {
    throw new ContractParseError('message page requires last_seq and has_more');
  }
  return { messages: input.messages.map((item, index) => parseMainMessage(item, index)), last_seq: input.last_seq, has_more: input.has_more };
}

function parseMainMessage(input: unknown, index: number): MainMessage {
  if (!isObject(input)) throw new ContractParseError(`messages[${index}] must be an object`);
  const role = input.role;
  if (role !== 'user' && role !== 'assistant' && role !== 'action') throw new ContractParseError('unknown main message role');
  if (typeof input.seq !== 'number' || input.seq < 1 || typeof input.id !== 'string' || input.id === '') {
    throw new ContractParseError('main message requires id and seq');
  }
  if (input.turn_status != null && input.turn_status !== '' && !TURN_STATUS.has(String(input.turn_status))) {
    throw new ContractParseError('unknown turn_status');
  }
  const message: MainMessage = {
    seq: input.seq,
    id: input.id,
    role,
    text: typeof input.text === 'string' ? input.text : undefined,
    turn_id: optionalUuid(input.turn_id, 'turn_id'),
    turn_status: typeof input.turn_status === 'string' ? input.turn_status : undefined,
    command_id: optionalUuid(input.command_id, 'command_id'),
    model_call_id: optionalUuid(input.model_call_id, 'model_call_id'),
    tool_call_id: optionalUuid(input.tool_call_id, 'tool_call_id'),
    work_item_id: optionalUuid(input.work_item_id, 'work_item_id'),
  };
  if (role === 'action' && !message.command_id && !message.model_call_id && !message.tool_call_id && !message.work_item_id) {
    throw new ContractParseError('action messages must reference a real command, model call, tool call, or work item');
  }
  return message;
}

export function parseCommandResult(input: unknown): CommandResult {
  if (!isObject(input)) throw new ContractParseError('command result must be an object');
  const status = input.status;
  if (status !== 'accepted' && status !== 'applied' && status !== 'rejected') throw new ContractParseError('unknown command status');
  if (typeof input.delivered !== 'boolean') throw new ContractParseError('delivered must be boolean');
  return {
    command_id: requiredString(input, 'command_id', 'command'),
    status,
    type: requiredString(input, 'type', 'command') as CommandType,
    epoch_number: requiredNumber(input, 'epoch_number', 'command'),
    work_item_id: optionalUuid(input.work_item_id, 'work_item_id'),
    attempt_id: optionalUuid(input.attempt_id, 'attempt_id'),
    delivered: input.delivered,
    result: input.result,
    error: parseError(input.error, 'command'),
  };
}

export function parseAcceptedTurn(input: unknown): { message_id: string; turn_id: string; session_id: string } {
  if (!isObject(input)) throw new ContractParseError('accepted turn must be an object');
  return {
    message_id: requiredString(input, 'message_id', 'accepted turn'),
    turn_id: requiredString(input, 'turn_id', 'accepted turn'),
    session_id: requiredString(input, 'session_id', 'accepted turn'),
  };
}
