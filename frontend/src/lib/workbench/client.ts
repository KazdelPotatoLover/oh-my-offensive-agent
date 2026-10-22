import {
  parseAcceptedTurn,
  parseCommandResult,
  parseEnvelope,
  parseMainMessagePage,
  parseWorkbenchSnapshot,
} from './parse';
import type {
  CommandBody,
  CommandResult,
  CommandType,
  MainMessagePage,
  WorkbenchErrorBody,
  WorkbenchSnapshot,
} from './parse';

export class WorkbenchRequestError extends Error {
  status: number;
  code: string;
  envelope?: WorkbenchErrorBody;

  constructor(message: string, status: number, code: string, envelope?: WorkbenchErrorBody) {
    super(message);
    this.name = 'WorkbenchRequestError';
    this.status = status;
    this.code = code;
    this.envelope = envelope;
  }
}

type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

export function workbenchSnapshotPath(runId: string, afterEventSeq = 0, limit = 50): string {
  return `/api/runs/${runId}/workbench?after_event_seq=${afterEventSeq}&limit=${limit}`;
}

export function mainMessagesPath(runId: string, afterSeq = 0, limit = 50): string {
  return `/api/runs/${runId}/main/messages?after_seq=${afterSeq}&limit=${limit}`;
}

export function commandsPath(runId: string): string {
  return `/api/runs/${runId}/commands`;
}

async function readBody(response: Response): Promise<unknown> {
  const text = await response.text();
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

function httpError(status: number, body: unknown): WorkbenchRequestError {
  const envelope = parseEnvelope(body);
  const fallback = typeof body === 'string' && body.trim() ? body.trim() : `HTTP ${status}`;
  return new WorkbenchRequestError(envelope?.message || fallback, status, envelope?.code || 'http_error', envelope || undefined);
}

export async function readSnapshot(fetchImpl: FetchLike, runId: string, query: { afterEventSeq?: number; limit?: number } = {}): Promise<WorkbenchSnapshot> {
  let response: Response;
  try {
    response = await fetchImpl(workbenchSnapshotPath(runId, query.afterEventSeq ?? 0, query.limit ?? 50), { headers: { Accept: 'application/json' } });
  } catch (err) {
    throw new WorkbenchRequestError(err instanceof Error ? err.message : 'network error', 0, 'disconnected');
  }
  const body = await readBody(response);
  if (!response.ok) throw httpError(response.status, body);
  return parseWorkbenchSnapshot(body);
}

async function readMainMessagePage(fetchImpl: FetchLike, runId: string, query: { afterSeq?: number; limit?: number } = {}): Promise<MainMessagePage> {
  let response: Response;
  try {
    response = await fetchImpl(mainMessagesPath(runId, query.afterSeq ?? 0, query.limit ?? 50), { headers: { Accept: 'application/json' } });
  } catch (err) {
    throw new WorkbenchRequestError(err instanceof Error ? err.message : 'network error', 0, 'disconnected');
  }
  const body = await readBody(response);
  if (!response.ok) throw httpError(response.status, body);
  return parseMainMessagePage(body);
}

async function submitMainMessage(
  fetchImpl: FetchLike,
  runId: string,
  body: { message: string; expected_epoch: number },
  idempotencyKey: string,
): Promise<{ httpStatus: number; body: { message_id: string; turn_id: string; session_id: string } }> {
  let response: Response;
  try {
    response = await fetchImpl(mainMessagesPath(runId).split('?')[0], {
      method: 'POST',
      headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify(body),
    });
  } catch (err) {
    throw new WorkbenchRequestError(err instanceof Error ? err.message : 'network error', 0, 'disconnected');
  }
  const raw = await readBody(response);
  if (!response.ok) throw httpError(response.status, raw);
  return { httpStatus: response.status, body: parseAcceptedTurn(raw) };
}

export const readMainMessages = Object.assign(readMainMessagePage, { submit: submitMainMessage });

export function buildCommand(input: {
  type: CommandType;
  expectedEpoch: number;
  workItemId?: string;
  expectedAttemptId?: string;
  payload?: Record<string, unknown>;
}): CommandBody {
  const command: CommandBody = { type: input.type, expected_epoch: input.expectedEpoch };
  if (input.workItemId) command.work_item_id = input.workItemId;
  if (input.expectedAttemptId) command.expected_attempt_id = input.expectedAttemptId;
  if (input.payload && Object.keys(input.payload).length > 0) command.payload = input.payload;
  return command;
}

export type CommandFeedback = {
  phase: 'accepted' | 'conflict' | 'disconnected' | 'rejected' | 'failed';
  canResend: boolean;
  httpStatus: number;
  error?: WorkbenchErrorBody;
};

export function classifyCommandResponse(status: number, body: unknown): CommandFeedback {
  if (status === 0) return { phase: 'disconnected', canResend: true, httpStatus: 0 };
  const envelope = parseEnvelope(body);
  if (status === 409) return { phase: 'conflict', canResend: false, httpStatus: status, error: envelope || undefined };
  if (status === 202 || status === 200) return { phase: 'accepted', canResend: false, httpStatus: status, error: envelope || undefined };
  return { phase: status >= 500 ? 'failed' : 'rejected', canResend: status === 0 || status >= 500, httpStatus: status, error: envelope || undefined };
}

export async function readCommandResult(
  fetchImpl: FetchLike,
  runId: string,
  command: CommandBody,
  idempotencyKey: string,
): Promise<{ httpStatus: number; body: CommandResult }> {
  let response: Response;
  try {
    response = await fetchImpl(commandsPath(runId), {
      method: 'POST',
      headers: { Accept: 'application/json', 'Content-Type': 'application/json', 'Idempotency-Key': idempotencyKey },
      body: JSON.stringify(command),
    });
  } catch (err) {
    throw new WorkbenchRequestError(err instanceof Error ? err.message : 'network error', 0, 'disconnected');
  }
  const raw = await readBody(response);
  if (!response.ok) throw httpError(response.status, raw);
  return { httpStatus: response.status, body: parseCommandResult(raw) };
}
