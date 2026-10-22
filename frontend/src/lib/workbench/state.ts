import type { MainMessage, MainMessagePage, WorkbenchSnapshot } from './parse';

export type QueryError = { status?: number; code?: string; message: string };

export type LoadState<T> = {
  status: 'idle' | 'loading' | 'ready' | 'error';
  data: T | null;
  error: QueryError | null;
  stale: boolean;
  generation: number;
};

export function initialLoadState<T>(): LoadState<T> {
  return { status: 'idle', data: null, error: null, stale: false, generation: 1 };
}

export function resetLoad<T>(generation: number): LoadState<T> {
  return { status: 'idle', data: null, error: null, stale: false, generation };
}

export function succeedLoad<T>(state: LoadState<T>, generation: number, data: T): LoadState<T> {
  if (generation !== state.generation) return state;
  return { status: 'ready', data, error: null, stale: false, generation };
}

export function failLoad<T>(state: LoadState<T>, generation: number, error: QueryError): LoadState<T> {
  if (generation !== state.generation) return state;
  return { status: 'error', data: state.data, error, stale: state.data != null, generation };
}

export function appendMessages(current: MainMessage[], page: MainMessagePage): MainMessage[] {
  const bySeq = new Map(current.map((message) => [message.seq, message]));
  for (const message of page.messages) bySeq.set(message.seq, message);
  const merged: MainMessage[] = [];
  bySeq.forEach((message) => merged.push(message));
  return merged.sort((left, right) => left.seq - right.seq);
}

export type SnapshotState = LoadState<WorkbenchSnapshot>;
