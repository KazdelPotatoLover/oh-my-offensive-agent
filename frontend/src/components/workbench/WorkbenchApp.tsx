'use client';

import React, { useEffect, useRef, useState } from 'react';
import Link from 'next/link';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import { ExplorationResources } from '@/components/ExplorationResources';
import { api, newRequestID } from '@/lib/api';
import type { Run, Task } from '@/lib/types';
import { executionGaps } from '@/lib/workbench/budget';
import {
  WorkbenchRequestError,
  buildCommand,
  classifyCommandResponse,
  readCommandResult,
  readMainMessages,
  readSnapshot,
} from '@/lib/workbench/client';
import { selectionFromPath, workbenchHref } from '@/lib/workbench/links';
import type { CommandBody, CommandType, MainMessage, WorkbenchSnapshot } from '@/lib/workbench/parse';
import {
  appendMessages,
  failLoad,
  initialLoadState,
  succeedLoad,
  type LoadState,
  type QueryError,
} from '@/lib/workbench/state';
import { CreateTaskForm } from './CreateTaskForm';
import { InspectorPane } from './InspectorPane';

const POLL_MS = 1500;
const fieldClass = 'rounded border border-gray-700 bg-gray-950 px-2 py-1 text-xs text-gray-100 focus:outline-none focus-visible:ring-2 focus-visible:ring-blue-500';

type Pane = 'nav' | 'execute' | 'inspect';
type RecordState<T> = { data: T | null; error: string | null };
type Submission = {
  phase: 'idle' | 'pending' | 'accepted' | 'conflict' | 'disconnected' | 'failed';
  httpStatus: number | null;
  message: string;
  canResend: boolean;
  key?: string;
  messageId?: string;
  turnId?: string;
  sessionId?: string;
};
type CommandFeedback = {
  phase: string;
  httpStatus: number | null;
  message: string;
  canResend: boolean;
  commandId?: string;
  workItemId?: string;
  delivered?: boolean;
  key?: string;
};

const idleSubmission: Submission = { phase: 'idle', httpStatus: null, message: '尚未提交', canResend: false };

function queryError(err: unknown): QueryError {
  if (err instanceof WorkbenchRequestError) return { status: err.status, code: err.code, message: err.message };
  return { message: err instanceof Error ? err.message : '请求失败' };
}

function envelopeOf(err: unknown): unknown {
  return err instanceof WorkbenchRequestError && err.envelope ? { error: err.envelope } : null;
}

async function readSnapshotPages(runId: string): Promise<WorkbenchSnapshot> {
  let merged: WorkbenchSnapshot | null = null;
  let after = 0;
  for (let page = 0; page < 8; page += 1) {
    const snap = await readSnapshot(fetch, runId, { afterEventSeq: after, limit: 50 });
    if (!merged) {
      merged = snap;
    } else {
      const latest = new Set(snap.events.map((event) => event.seq));
      const events = [...merged.events.filter((event) => !latest.has(event.seq)), ...snap.events].sort((left, right) => left.seq - right.seq);
      merged = { ...snap, events };
    }
    if (!snap.has_more_events) return merged;
    after = snap.last_event_seq;
  }
  return merged as WorkbenchSnapshot;
}

export function WorkbenchApp() {
  const pathname = usePathname() || '/workbench';
  const searchParams = useSearchParams();
  const selection = selectionFromPath(pathname, searchParams?.toString() || '');
  return <WorkbenchSession key={selection.runId || 'none'} />;
}

function WorkbenchSession() {
  const pathname = usePathname() || '/workbench';
  const searchParams = useSearchParams();
  const router = useRouter();
  const selection = selectionFromPath(pathname, searchParams?.toString() || '');
  const runId = selection.runId;

  const generationRef = useRef(1);
  const snapshotHolder = useRef<LoadState<WorkbenchSnapshot>>(initialLoadState());
  const [snapshot, setSnapshot] = useState(snapshotHolder.current);
  const [messages, setMessages] = useState<MainMessage[]>([]);
  const [messageError, setMessageError] = useState<QueryError | null>(null);
  const [tasks, setTasks] = useState<RecordState<Task[]>>({ data: null, error: null });
  const [runs, setRuns] = useState<RecordState<Run[]>>({ data: null, error: null });
  const [runRecord, setRunRecord] = useState<RecordState<Run>>({ data: null, error: null });
  const [taskRecord, setTaskRecord] = useState<RecordState<Task>>({ data: null, error: null });
  const [modelCalls, setModelCalls] = useState<{ count: number | null; error: string | null }>({ count: null, error: null });
  const [pane, setPane] = useState<Pane>(selection.workItemId ? 'inspect' : runId ? 'execute' : 'nav');
  const [creating, setCreating] = useState(false);
  const [draft, setDraft] = useState('');
  const [idemKey, setIdemKey] = useState(newRequestID);
  const [submission, setSubmission] = useState<Submission>(idleSubmission);
  const [lastSubmit, setLastSubmit] = useState<{ key: string; body: { message: string; expected_epoch: number } } | null>(null);
  const [commandFeedback, setCommandFeedback] = useState<CommandFeedback | null>(null);
  const [commandPending, setCommandPending] = useState(false);
  const [lastCommand, setLastCommand] = useState<{ key: string; body: CommandBody } | null>(null);
  const [epochReason, setEpochReason] = useState('');
  const [epochSummary, setEpochSummary] = useState('');
  const [grant, setGrant] = useState({ model: 0, tool: 0, seconds: 0 });
  const [hint, setHint] = useState('');
  const [intent, setIntent] = useState('');

  useEffect(() => {
    let cancelled = false;
    api.getTasks()
      .then((list) => { if (!cancelled) setTasks({ data: list, error: null }); })
      .catch((err: unknown) => { if (!cancelled) setTasks({ data: null, error: err instanceof Error ? err.message : '任务读取失败' }); });
    return () => { cancelled = true; };
  }, []);

  useEffect(() => {
    if (!runId) return;
    let cancelled = false;
    api.getRun(runId)
      .then((run) => { if (!cancelled) setRunRecord({ data: run, error: null }); })
      .catch((err: unknown) => { if (!cancelled) setRunRecord({ data: null, error: err instanceof Error ? err.message : '运行记录读取失败' }); });
    return () => { cancelled = true; };
  }, [runId]);

  const taskId = selection.taskId || runRecord.data?.task_id;

  useEffect(() => {
    if (!taskId) return;
    let cancelled = false;
    setRuns({ data: null, error: null });
    setTaskRecord({ data: null, error: null });
    api.getTask(taskId)
      .then((task) => { if (!cancelled) setTaskRecord({ data: task, error: null }); })
      .catch((err: unknown) => { if (!cancelled) setTaskRecord({ data: null, error: err instanceof Error ? err.message : '任务详情读取失败' }); });
    api.getRuns(taskId)
      .then((list) => { if (!cancelled) setRuns({ data: list, error: null }); })
      .catch((err: unknown) => { if (!cancelled) setRuns({ data: null, error: err instanceof Error ? err.message : '运行列表读取失败' }); });
    return () => { cancelled = true; };
  }, [taskId]);

  useEffect(() => {
    if (!runId) return;
    let cancelled = false;
    const load = async () => {
      try {
        const response = await fetch(`/api/runs/${runId}/model_calls`, { headers: { Accept: 'application/json' } });
        const text = await response.text();
        const body = text ? JSON.parse(text) as { model_calls?: unknown; error?: unknown } : null;
        if (!response.ok) throw new Error(typeof body?.error === 'string' ? body.error : `模型调用记录 HTTP ${response.status}`);
        if (!body || !Array.isArray(body.model_calls)) throw new Error('模型调用记录缺少数组');
        if (!cancelled) setModelCalls({ count: body.model_calls.length, error: null });
      } catch (err) {
        if (!cancelled) setModelCalls({ count: null, error: err instanceof Error ? err.message : '模型调用记录读取失败' });
      }
    };
    void load();
    return () => { cancelled = true; };
  }, [runId, snapshot.data?.snapshot_at]);

  useEffect(() => {
    const generation = generationRef.current;
    if (!runId) return;
    let stopped = false;
    let snapState = snapshotHolder.current;
    let currentMessages: MainMessage[] = [];
    let cursor = 0;
    const tick = async () => {
      if (stopped || generation !== generationRef.current) return;
      try {
        const snap = await readSnapshotPages(runId);
        if (stopped || generation !== generationRef.current) return;
        snapState = succeedLoad(snapState, generation, snap);
        snapshotHolder.current = snapState;
        setSnapshot(snapState);
      } catch (err) {
        if (stopped || generation !== generationRef.current) return;
        snapState = failLoad(snapState, generation, queryError(err));
        snapshotHolder.current = snapState;
        setSnapshot(snapState);
      }
      try {
        for (let page = 0; page < 8; page += 1) {
          const next = await readMainMessages(fetch, runId, { afterSeq: cursor, limit: 50 });
          if (stopped || generation !== generationRef.current) return;
          currentMessages = appendMessages(currentMessages, next);
          cursor = next.last_seq;
          setMessages(currentMessages);
          setMessageError(null);
          if (!next.has_more) break;
        }
      } catch (err) {
        if (stopped || generation !== generationRef.current) return;
        setMessageError(queryError(err));
      }
    };
    void tick();
    const timer = window.setInterval(() => { void tick(); }, POLL_MS);
    return () => {
      stopped = true;
      window.clearInterval(timer);
    };
  }, [runId]);

  useEffect(() => {
    if (!selection.workItemId) return;
    document.getElementById('inspector-heading')?.focus();
  }, [selection.workItemId, pane]);

  const snap = snapshot.data;
  const direction = snap?.directions.find((item) => item.work_item_id === selection.workItemId);
  const gaps = taskRecord.data && runRecord.data ? executionGaps(taskRecord.data.config, runRecord.data.execution_input) : [];
  const runStatus = snap?.run.status || '';
  const canCommand = Boolean(snap) && !commandPending;
  const canPause = canCommand && (runStatus === 'running' || runStatus === 'queued');
  const canStop = canCommand && ['running', 'queued', 'paused', 'stopping'].includes(runStatus);
  const canResume = canCommand && runStatus === 'paused';
  const canEpoch = canCommand && ['stopped', 'completed', 'failed', 'interrupted'].includes(runStatus);

  const openWork = (workItemId: string) => {
    setPane('inspect');
    if (taskId && runId) router.push(workbenchHref({ taskId, runId, workItemId }));
    else if (runId) router.push(`/workbench?run=${encodeURIComponent(runId)}&work=${encodeURIComponent(workItemId)}`);
  };

  const submitMessage = async (key: string, body: { message: string; expected_epoch: number }) => {
    if (!runId) return;
    setSubmission({ phase: 'pending', httpStatus: null, message: '正在提交，尚未收到响应', canResend: false, key });
    setLastSubmit({ key, body });
    try {
      const result = await readMainMessages.submit(fetch, runId, body, key);
      setSubmission({
        phase: 'accepted',
        httpStatus: result.httpStatus,
        message: `HTTP ${result.httpStatus} · 已接受 · 回复尚未到达`,
        canResend: false,
        key,
        messageId: result.body.message_id,
        turnId: result.body.turn_id,
        sessionId: result.body.session_id,
      });
      setDraft('');
      setIdemKey(newRequestID());
    } catch (err) {
      const status = err instanceof WorkbenchRequestError ? err.status : 0;
      const classified = classifyCommandResponse(status, envelopeOf(err));
      setSubmission({
        phase: status === 409 ? 'conflict' : status === 0 ? 'disconnected' : 'failed',
        httpStatus: status,
        message: err instanceof Error ? err.message : '提交失败',
        canResend: classified.canResend,
        key,
      });
    }
  };

  const sendCommand = async (body: CommandBody, key = newRequestID()) => {
    if (!runId || !snap) return;
    setCommandPending(true);
    setLastCommand({ key, body });
    setCommandFeedback({ phase: 'pending', httpStatus: null, message: `正在提交 ${body.type}`, canResend: false, key, workItemId: body.work_item_id });
    try {
      const result = await readCommandResult(fetch, runId, body, key);
      setCommandFeedback({
        phase: result.body.status,
        httpStatus: result.httpStatus,
        delivered: result.body.delivered,
        commandId: result.body.command_id,
        workItemId: result.body.work_item_id,
        key,
        canResend: false,
        message: result.body.delivered
          ? `HTTP ${result.httpStatus} · ${result.body.status} · delivered=true · ${result.body.command_id}`
          : `HTTP ${result.httpStatus} · ${result.body.status} · delivered=false · 已接受，Worker 尚未读取 · ${result.body.command_id}`,
      });
    } catch (err) {
      const status = err instanceof WorkbenchRequestError ? err.status : 0;
      const classified = classifyCommandResponse(status, envelopeOf(err));
      setCommandFeedback({
        phase: classified.phase,
        httpStatus: status,
        message: err instanceof Error ? err.message : '命令失败',
        canResend: classified.canResend,
        key,
        workItemId: body.work_item_id,
      });
    } finally {
      setCommandPending(false);
    }
  };

  const commandFor = (type: CommandType, payload?: Record<string, unknown>, workItemId?: string, attemptId?: string): CommandBody | null => {
    if (!snap) return null;
    return buildCommand({
      type,
      expectedEpoch: snap.run.epoch_number,
      workItemId,
      expectedAttemptId: attemptId,
      payload,
    });
  };

  return (
    <div lang="zh-CN" data-testid="workbench-shell" className="fixed inset-x-0 bottom-0 top-16 z-40 flex flex-col bg-gray-950 text-gray-100">
      <h1 className="sr-only">统一工作台</h1>
      <header className="shrink-0 border-b border-gray-800 px-3 py-2">
        <div className="flex flex-wrap items-start justify-between gap-2">
          <div className="min-w-0 space-y-1 text-xs">
            <p data-testid="run-status">Run 状态 {snap ? snap.run.status : '无 workbench.v1 快照'}</p>
            <p data-testid="run-epoch">Epoch {snap ? snap.run.epoch_number : '无'}{snap ? ` · 快照 ${snap.snapshot_at}` : ''}</p>
            {runRecord.data && <p data-testid="legacy-epoch">记录 current_epoch {runRecord.data.current_epoch}。命令只使用快照 Epoch。</p>}
            {snap && (
              <>
                <p data-testid="worker-capacity">Worker 池 requested {snap.worker_capacity.requested} / effective {snap.worker_capacity.effective} / active {snap.worker_capacity.active} / queued {snap.worker_capacity.queued}</p>
                <p data-testid="worker-count">方向 {snap.directions.length}</p>
                <p data-testid="model-capacity">模型请求并发 limit {snap.model_capacity.limit} / in_flight {snap.model_capacity.in_flight} / queued {snap.model_capacity.queued}。这不是 Worker 数量。</p>
                <p data-testid="planner-status">Planner {snap.planner.status}{snap.planner.reason ? ` · ${snap.planner.reason}` : ''}{snap.planner.session_id ? ` · session ${snap.planner.session_id}` : ''}</p>
                {snap.origin_pacing && <p data-testid="origin-pacing">源站等待 {snap.origin_pacing.waiting ? '是' : '否'}{snap.origin_pacing.reason ? ` · ${snap.origin_pacing.reason}` : ''}。等待不改写 Worker effective。</p>}
              </>
            )}
            <p data-testid="model-call-count">{modelCalls.error ? `模型调用记录读取失败：${modelCalls.error}` : `模型调用记录 ${modelCalls.count == null ? '未知' : modelCalls.count} 条，不是 Worker 卡片。`}</p>
          </div>
          <div className="flex max-w-xl flex-wrap gap-1">
            <button type="button" data-testid="pause-run" disabled={!canPause} onClick={() => { const body = commandFor('pause_run'); if (body) void sendCommand(body); }} className={`${fieldClass} disabled:opacity-40`}>暂停</button>
            <button type="button" data-testid="resume-run" disabled={!canResume} onClick={() => { const body = commandFor('resume_run'); if (body) void sendCommand(body); }} className={`${fieldClass} disabled:opacity-40`}>恢复</button>
            <button type="button" data-testid="stop-run" disabled={!canStop} onClick={() => { const body = commandFor('stop_run'); if (body) void sendCommand(body); }} className={`${fieldClass} disabled:opacity-40`}>停止</button>
            <label className="text-[11px] text-gray-400">代次原因
              <input data-testid="epoch-reason" value={epochReason} onChange={(event) => setEpochReason(event.target.value)} className={`${fieldClass} ml-1`} />
            </label>
            <label className="text-[11px] text-gray-400">摘要
              <input aria-label="代次摘要" value={epochSummary} onChange={(event) => setEpochSummary(event.target.value)} className={`${fieldClass} ml-1`} />
            </label>
            <button type="button" data-testid="start-epoch" disabled={!canEpoch || epochReason.trim() === ''} onClick={() => { const body = commandFor('start_epoch', { reason: epochReason.trim(), ...(epochSummary.trim() ? { summary: epochSummary.trim() } : {}) }); if (body) void sendCommand(body); }} className={`${fieldClass} disabled:opacity-40`}>新 Epoch</button>
            <label className="text-[11px] text-gray-400">追加模型
              <input data-testid="grant-model-calls" type="number" value={grant.model} onChange={(event) => setGrant((current) => ({ ...current, model: Number(event.target.value) }))} className={`${fieldClass} ml-1 w-16`} />
            </label>
            <label className="text-[11px] text-gray-400">追加工具
              <input aria-label="追加工具次数" type="number" value={grant.tool} onChange={(event) => setGrant((current) => ({ ...current, tool: Number(event.target.value) }))} className={`${fieldClass} ml-1 w-16`} />
            </label>
            <label className="text-[11px] text-gray-400">追加秒
              <input aria-label="追加秒数" type="number" value={grant.seconds} onChange={(event) => setGrant((current) => ({ ...current, seconds: Number(event.target.value) }))} className={`${fieldClass} ml-1 w-16`} />
            </label>
            <button type="button" data-testid="grant-budget" disabled={!canCommand} onClick={() => { const body = commandFor('grant_budget', { additional_model_calls: grant.model, additional_tool_calls: grant.tool, additional_seconds: grant.seconds }); if (body) void sendCommand(body); }} className={`${fieldClass} disabled:opacity-40`}>追加授权</button>
          </div>
        </div>
        {snap && <pre data-testid="budget-summary" className="mt-1 max-h-16 overflow-auto whitespace-pre-wrap text-[11px] text-gray-400">额度 {JSON.stringify(snap.budget)}</pre>}
        {snapshot.stale && <p role="alert" data-testid="stale-snapshot" className="mt-1 text-xs text-amber-200">上次快照仍保留。{snapshot.error?.message}</p>}
        {snapshot.status === 'error' && !snapshot.data && (
          <p role="alert" data-testid="workbench-unavailable" className="mt-1 text-xs text-red-200">
            workbench.v1 不可用{snapshot.error?.code ? ` · ${snapshot.error.code}` : ''} · {snapshot.error?.message}。未改走旧的 pause/steer 接口。
          </p>
        )}
        {commandFeedback && (
          <p role="status" data-testid="command-feedback" className="mt-1 text-xs text-gray-200">
            {commandFeedback.message}
            {commandFeedback.workItemId ? ` · work_item ${commandFeedback.workItemId}` : ''}
            {commandFeedback.key ? ` · Idempotency-Key ${commandFeedback.key}` : ''}
          </p>
        )}
        {commandFeedback?.canResend && lastCommand && (
          <button type="button" className="mt-1 text-xs text-blue-300" onClick={() => void sendCommand(lastCommand.body, lastCommand.key)}>用同一把钥匙重发命令</button>
        )}
        <p className="mt-1 text-[11px] text-gray-500">范围变更不在统一命令里。危险的预算和代次变更使用上面的结构化输入。accepted 不等于 delivered。</p>
      </header>
      <div role="tablist" aria-label="工作台分区" className="flex shrink-0 gap-1 border-b border-gray-800 px-2 py-1 lg:hidden">
        {([
          ['nav', '导航'],
          ['execute', '执行'],
          ['inspect', '检查'],
        ] as const).map(([id, label]) => (
          <button key={id} type="button" role="tab" aria-selected={pane === id} data-testid={`pane-${id}`} onClick={() => setPane(id)} className={`rounded px-2 py-1 text-xs ${pane === id ? 'bg-gray-800 text-white' : 'text-gray-400'}`}>{label}</button>
        ))}
      </div>
      <div className="flex min-h-0 flex-1">
        <aside className={`${pane === 'nav' ? 'flex' : 'hidden'} min-h-0 w-full shrink-0 flex-col overflow-auto border-r border-gray-800 lg:flex lg:w-64`}>
          <div className="flex items-center justify-between px-3 py-2">
            <h2 className="text-sm font-semibold">任务</h2>
            <button type="button" data-testid="open-create" onClick={() => { setCreating(true); setPane('execute'); }} className="text-xs text-blue-300">新任务</button>
          </div>
          {tasks.error && <p role="alert" className="px-3 text-xs text-red-200">{tasks.error}</p>}
          {tasks.data === null && !tasks.error && <p className="px-3 text-xs text-gray-400">正在读取任务</p>}
          {tasks.data && tasks.data.length === 0 && <p className="px-3 text-xs text-gray-400">没有任务。</p>}
          <ul className="space-y-1 px-2">
            {(tasks.data || []).map((task) => (
              <li key={task.id}>
                <Link data-testid="task-link" href={workbenchHref({ taskId: task.id })} className={`block rounded px-2 py-1 text-xs ${task.id === taskId ? 'bg-gray-800 text-white' : 'text-gray-300'}`}>{task.name}</Link>
              </li>
            ))}
          </ul>
          <h2 className="px-3 pt-3 text-sm font-semibold">Run</h2>
          {runs.error && <p role="alert" className="px-3 text-xs text-red-200">{runs.error}</p>}
          {taskId && runs.data === null && !runs.error && <p className="px-3 text-xs text-gray-400">正在读取运行</p>}
          <ul className="space-y-1 px-2">
            {(runs.data || []).map((run) => (
              <li key={run.id}>
                <Link data-testid="run-link" href={workbenchHref({ taskId: run.task_id, runId: run.id })} className={`block rounded px-2 py-1 text-xs ${run.id === runId ? 'bg-gray-800 text-white' : 'text-gray-300'}`}>
                  {run.id.slice(0, 8)} · 记录 {run.status}
                </Link>
              </li>
            ))}
          </ul>
          {snap && <p className="px-3 pt-3 text-xs text-gray-300">Planner {snap.planner.status}</p>}
          {runId && taskId && <Link data-testid="old-run-link" href={`/tasks/${taskId}/runs/${runId}`} className="px-3 py-2 text-xs text-blue-300">旧运行页</Link>}
          {runRecord.error && <p role="alert" className="px-3 text-xs text-red-200">{runRecord.error}</p>}
          {snapshot.status === 'error' && !snap && runRecord.data && <p className="px-3 text-xs text-amber-200">这是旧运行记录，不是 workbench.v1。统一命令已禁用。</p>}
        </aside>
        <section className={`${pane === 'execute' ? 'flex' : 'hidden'} min-h-0 min-w-0 flex-1 flex-col overflow-hidden lg:flex`}>
          {creating ? (
            <div className="p-3"><CreateTaskForm onCancel={() => setCreating(false)} /></div>
          ) : !runId ? (
            <div className="p-4 text-sm text-gray-300">
              <p>选择任务和 Run，或创建新的探索任务。</p>
              <button type="button" onClick={() => setCreating(true)} className="mt-2 rounded bg-blue-600 px-3 py-1.5 text-xs">新任务</button>
            </div>
          ) : (
            <div className="flex min-h-0 flex-1 flex-col">
              {(taskRecord.error || (taskRecord.data && runRecord.data && gaps.length > 0)) && (
                <div data-testid="execution-gaps" className="border-b border-gray-800 px-3 py-2 text-xs text-amber-200">
                  {taskRecord.error && <p role="alert">{taskRecord.error}</p>}
                  {gaps.length > 0 && <p>执行输入未保留：{gaps.join(', ')}。界面不会把这些字段显示成已经生效。</p>}
                </div>
              )}
              <div data-testid="direction-board" className="grid max-h-52 shrink-0 gap-2 overflow-auto border-b border-gray-800 p-3 sm:grid-cols-2">
                {snapshot.status === 'loading' && !snap && <p>正在读取工作台快照</p>}
                {snap && snap.directions.length === 0 && <p data-testid="direction-empty">当前快照没有方向。</p>}
                {snap?.directions.map((item) => (
                  <article key={item.work_item_id} data-testid="direction-card" data-work-item-id={item.work_item_id} data-status={item.status} className={`rounded border px-2 py-2 text-xs ${item.work_item_id === selection.workItemId ? 'border-blue-500' : 'border-gray-800'}`}>
                    <span data-testid={`direction-card-${item.work_item_id}`} className="sr-only">{item.work_item_id}</span>
                    <button type="button" onClick={() => openWork(item.work_item_id)} className="text-left text-sm font-medium text-white focus:outline-none focus-visible:ring-2 focus-visible:ring-blue-500">
                      {item.objective || '未提供目标'}
                    </button>
                    <p>状态 {item.status}</p>
                    <p>来源 {item.source_refs?.length ? item.source_refs.join(', ') : '未提供'}</p>
                    <p>最新步骤 {item.latest_step || '未提供'}</p>
                    <p className="font-mono">Attempt {item.attempt_id || '无'}</p>
                    <p className="break-all font-mono">局部额度 {item.local_budget ? JSON.stringify(item.local_budget) : '未提供'}</p>
                    {item.error && <p>失败范围 {item.error.scope} · {item.error.code} · {item.error.message}</p>}
                  </article>
                ))}
              </div>
              <div data-testid="main-conversation" className="min-h-0 flex-1 overflow-auto px-3 py-2" aria-live="polite">
                <h2 className="text-sm font-semibold">Main Agent</h2>
                {messageError && <p role="alert" className="text-xs text-red-200">{messageError.message}</p>}
                {messages.map((message) => (
                  <article key={message.seq} data-testid="message-row" data-command-id={message.command_id || undefined} className="border-b border-gray-800 py-1 text-xs">
                    <p>{message.role} · seq {message.seq}{message.turn_status ? ` · ${message.turn_status}` : ''}</p>
                    {message.text && <p>{message.text}</p>}
                    <p className="font-mono text-gray-400">
                      {message.command_id ? `command ${message.command_id}` : ''}
                      {message.model_call_id ? ` model_call ${message.model_call_id}` : ''}
                      {message.tool_call_id ? ` tool_call ${message.tool_call_id}` : ''}
                      {message.work_item_id ? ` work_item ${message.work_item_id}` : ''}
                    </p>
                  </article>
                ))}
                {messages.length === 0 && !messageError && <p className="text-xs text-gray-400">还没有主对话消息。</p>}
              </div>
              <form className="shrink-0 border-t border-gray-800 p-3" onSubmit={(event) => {
                event.preventDefault();
                if (!snap || !draft.trim() || submission.phase === 'pending') return;
                void submitMessage(idemKey, { message: draft.trim(), expected_epoch: snap.run.epoch_number });
              }}>
                <label className="block text-xs text-gray-300">给 Main Agent 的消息
                  <textarea aria-label="给 Main Agent 的消息" value={draft} onChange={(event) => { setDraft(event.target.value); setIdemKey(newRequestID()); }} rows={2} className={`${fieldClass} mt-1 w-full`} />
                </label>
                <div className="mt-2 flex flex-wrap gap-2">
                  <button type="submit" data-testid="send-message" disabled={!snap || !draft.trim() || submission.phase === 'pending'} className="rounded bg-blue-600 px-3 py-1.5 text-xs disabled:opacity-40">发送</button>
                  {submission.canResend && lastSubmit && <button type="button" data-testid="resend-message" onClick={() => void submitMessage(lastSubmit.key, lastSubmit.body)} className="text-xs text-blue-300">重发上一请求</button>}
                  {lastSubmit && draft.trim() && draft.trim() !== lastSubmit.body.message && (
                    <button type="button" data-testid="conflict-resend" onClick={() => void submitMessage(lastSubmit.key, { message: draft.trim(), expected_epoch: lastSubmit.body.expected_epoch })} className="text-xs text-amber-200">用上一把钥匙提交当前草稿</button>
                  )}
                </div>
                <p role="status" data-testid="submission-status" className="mt-2 text-xs text-gray-200">
                  {submission.message}
                  {submission.httpStatus != null ? ` · HTTP ${submission.httpStatus}` : ''}
                  {submission.messageId ? ` · message ${submission.messageId}` : ''}
                  {submission.turnId ? ` · turn ${submission.turnId}` : ''}
                  {submission.sessionId ? ` · session ${submission.sessionId}` : ''}
                  {submission.key ? ` · key ${submission.key}` : ''}
                </p>
              </form>
              <div data-testid="activity-stream" className="max-h-28 overflow-auto border-t border-gray-800 px-3 py-2 text-xs">
                <h2 className="font-medium">活动</h2>
                {snap?.events.map((event) => <p key={event.seq}>{event.seq} · {event.type} · {event.payload ? JSON.stringify(event.payload) : ''}</p>)}
                {snap && snap.events.length === 0 && <p>快照没有事件。</p>}
                {snap?.has_more_events && <p>事件页未读完。</p>}
              </div>
              <details className="border-t border-gray-800 px-3 py-2 text-xs">
                <summary>Planner 输入</summary>
                <label className="mt-2 block">hint
                  <input aria-label="hint" value={hint} onChange={(event) => setHint(event.target.value)} className={`${fieldClass} mt-1 w-full`} />
                </label>
                <button type="button" disabled={!canCommand || !hint.trim()} onClick={() => { const body = commandFor('hint', { text: hint.trim() }); if (body) void sendCommand(body); }} className="mt-1 rounded border border-gray-700 px-2 py-1 disabled:opacity-40">提交 hint</button>
                <label className="mt-2 block">add_intent
                  <input aria-label="add_intent" value={intent} onChange={(event) => setIntent(event.target.value)} className={`${fieldClass} mt-1 w-full`} />
                </label>
                <button type="button" disabled={!canCommand || !intent.trim()} onClick={() => { const body = commandFor('add_intent', { summary: intent.trim() }); if (body) void sendCommand(body); }} className="mt-1 rounded border border-gray-700 px-2 py-1 disabled:opacity-40">提交意图</button>
              </details>
            </div>
          )}
        </section>
        <aside className={`${pane === 'inspect' ? 'flex' : 'hidden'} min-h-0 w-full shrink-0 flex-col overflow-hidden border-l border-gray-800 lg:flex lg:w-96`}>
          {runId ? (
            <>
              <InspectorPane
                runId={runId}
                workItemId={selection.workItemId}
                direction={direction}
                snapshotAt={snap?.snapshot_at}
                runStatus={runStatus || runRecord.data?.status || ''}
                commandPending={commandPending || !snap}
                showLegacy={snapshot.status === 'error' && !snap && Boolean(runRecord.data)}
                onCommand={(type, payload) => {
                  const body = commandFor(type, payload, selection.workItemId, direction?.attempt_id);
                  if (body) void sendCommand(body);
                }}
              />
              <div className="max-h-40 overflow-auto border-t border-gray-800">
                <ExplorationResources runId={runId} />
              </div>
            </>
          ) : (
            <p className="p-3 text-xs text-gray-400">选择 Run 后查看检查器。</p>
          )}
        </aside>
      </div>
    </div>
  );
}
