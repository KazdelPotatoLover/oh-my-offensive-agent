'use client';

import React, { useEffect, useMemo, useRef, useState } from 'react';
import Link from 'next/link';
import { usePathname, useRouter, useSearchParams } from 'next/navigation';
import {
  Activity,
  Brain,
  ChevronDown,
  ChevronRight,
  ChevronUp,
  Database,
  Layers,
  LayoutGrid,
  Maximize2,
  MessageSquare,
  Network,
  PanelLeftClose,
  PanelLeftOpen,
  Pause,
  Play,
  Radio,
  Send,
  Sliders,
  Square,
} from 'lucide-react';
import { ExplorationResources } from '@/components/ExplorationResources';
import { api, newRequestID } from '@/lib/api';
import type { Run, Task } from '@/lib/types';
import { cn } from '@/lib/utils';
import { runNotice } from '@/lib/workbench/run-notice';
import { budgetSummaryText, executionGaps, limitFlags } from '@/lib/workbench/budget';
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
import { ExplorationCanvas } from './ExplorationCanvas';
import { buildExplorationGraph, type ExplorationGraph } from '@/lib/workbench/graph';
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
  const sessionKey = `${selection.taskId || 'none'}:${selection.runId || 'none'}`;
  return <WorkbenchSession key={sessionKey} />;
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
  const [activeView, setActiveView] = useState<'execute' | 'graph'>('execute');
  const [workbenchTab, setWorkbenchTab] = useState<'main' | 'inspector' | 'directions' | 'activity' | 'graph' | 'resources'>(
    selection.workItemId ? 'inspector' : 'main'
  );
  const [layoutMode, setLayoutMode] = useState<'tabs' | 'split'>('tabs');
  const [sidebarOpen, setSidebarOpen] = useState(true);
  const [activityOpen, setActivityOpen] = useState(false);
  const [directionBoardOpen, setDirectionBoardOpen] = useState(true);
  const [showAdvancedControls, setShowAdvancedControls] = useState(false);
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
  const [graph, setGraph] = useState<ExplorationGraph | null>(null);
  const [graphError, setGraphError] = useState<string | null>(null);
  const [startingRun, setStartingRun] = useState(false);
  const [startRunError, setStartRunError] = useState<string | null>(null);

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
    if (!taskId || runId) return;
    if (runs.data && runs.data.length > 0) {
      const latest = runs.data[0];
      router.replace(workbenchHref({ taskId, runId: latest.id }));
    }
  }, [taskId, runId, runs.data, router]);

  const handleStartRun = async () => {
    if (!taskId || startingRun) return;
    setStartingRun(true);
    setStartRunError(null);
    try {
      const run = await api.createRun(taskId, {
        idempotency_key: newRequestID(),
        executor: 'orchestrator',
      });
      router.push(workbenchHref({ taskId, runId: run.id }));
    } catch (err: unknown) {
      setStartRunError(err instanceof Error ? err.message : '启动运行失败');
    } finally {
      setStartingRun(false);
    }
  };

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
    if (!runId) return;
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout>;
    setGraph(null);
    setGraphError(null);
    const loadGraph = async () => {
      try {
        const response = await api.getExplorationGraph(runId);
        if (!cancelled) {
          setGraph(prev => JSON.stringify(prev) === JSON.stringify(response) ? prev : response);
          setGraphError(null);
        }
      } catch (err) {
        if (!cancelled) setGraphError(err instanceof Error ? err.message : '探索图读取失败');
      }
      if (!cancelled) timer = setTimeout(() => { void loadGraph(); }, 4000);
    };
    void loadGraph();
    return () => {
      cancelled = true;
      clearTimeout(timer);
    };
  }, [runId]);

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
    if (selection.workItemId) {
      setWorkbenchTab('inspector');
      setPane('inspect');
      document.getElementById('inspector-heading')?.focus();
    } else if (runId) {
      setWorkbenchTab('main');
      setPane('execute');
    }
  }, [selection.workItemId, runId]);

  useEffect(() => {
    if (snapshot.status === 'error' && !snapshot.data && runRecord.data) {
      setWorkbenchTab('inspector');
      setPane('inspect');
    }
  }, [snapshot.status, snapshot.data, runRecord.data]);

  const snap = snapshot.data;
  const direction = snap?.directions.find((item) => item.work_item_id === selection.workItemId);
  const gaps = taskRecord.data && runRecord.data ? executionGaps(taskRecord.data.config, runRecord.data.execution_input) : [];
  const runStatus = snap?.run.status || '';
  const canCommand = Boolean(snap) && !commandPending;
  const canPause = canCommand && (runStatus === 'running' || runStatus === 'queued');
  const canStop = canCommand && ['running', 'queued', 'paused', 'stopping'].includes(runStatus);
  const canResume = canCommand && runStatus === 'paused';
  const canEpoch = canCommand && ['stopped', 'failed', 'interrupted'].includes(runStatus);
  const terminationNotice = runNotice(runRecord.data);

  const { graphNodes, graphEdges } = useMemo(() => buildExplorationGraph(graph), [graph]);

  const openWork = (workItemId: string) => {
    setPane('inspect');
    setWorkbenchTab('inspector');
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
    <div lang="zh-CN" data-testid="workbench-shell" className="fixed inset-x-0 bottom-0 top-16 z-40 flex flex-col overflow-y-auto bg-gray-950 text-gray-100">
      <h1 className="sr-only">统一工作台</h1>

      {/* Sleek Modern Header */}
      {runId && !creating && <header className="shrink-0 border-b border-gray-800 bg-gray-950/80 px-4 py-2 backdrop-blur">
        {/* Top row: Status & Actions */}
        <div className="flex flex-wrap items-center justify-between gap-3">
          {/* Status Pills */}
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <div className="inline-flex items-center gap-1.5 rounded-full border border-gray-800 bg-gray-900/90 px-2.5 py-1">
              <span className={`inline-block size-2 rounded-full ${runStatus === 'running' ? 'bg-emerald-400 animate-pulse' : runStatus === 'paused' ? 'bg-amber-400' : 'bg-gray-400'}`} />
              <span data-testid="run-status" className="font-semibold text-gray-200">
                Run 状态 {snap ? snap.run.status : '无 workbench.v1 快照'}
              </span>
            </div>

            <div className="inline-flex items-center gap-1 rounded-md border border-gray-800 bg-gray-900 px-2 py-1 font-mono text-[11px] text-gray-300">
              <span data-testid="run-epoch">Epoch {snap ? snap.run.epoch_number : '无'}{snap ? ` · 快照 ${snap.snapshot_at}` : ''}</span>
            </div>

            {snap && (
              <div className="inline-flex items-center gap-1 rounded-md border border-gray-800 bg-gray-900 px-2 py-1 text-[11px] text-gray-300">
                <Brain className="size-3 text-purple-400" />
                <span data-testid="planner-status">
                  Planner {snap.planner.status}{snap.planner.reason ? ` · ${snap.planner.reason}` : ''}{snap.planner.session_id ? ` · session ${snap.planner.session_id}` : ''}
                </span>
              </div>
            )}

            {runRecord.data && (
              <span data-testid="legacy-epoch" className="text-[11px] text-gray-500">
                记录 current_epoch {runRecord.data.current_epoch}。命令只使用快照 Epoch。
              </span>
            )}
          </div>

          {/* Action buttons */}
          <div className="flex flex-wrap items-center gap-2">
            <div className="flex items-center rounded-lg border border-gray-800 bg-gray-900 p-0.5 shadow-sm">
              <button
                type="button"
                data-testid="pause-run"
                disabled={!canPause}
                onClick={() => { const body = commandFor('pause_run'); if (body) void sendCommand(body); }}
                className="flex items-center gap-1 rounded px-2.5 py-1 text-xs font-medium text-amber-300 hover:bg-amber-500/10 disabled:opacity-40"
              >
                <Pause className="size-3" />
                暂停
              </button>
              <button
                type="button"
                data-testid="resume-run"
                disabled={!canResume}
                onClick={() => { const body = commandFor('resume_run'); if (body) void sendCommand(body); }}
                className="flex items-center gap-1 rounded px-2.5 py-1 text-xs font-medium text-blue-300 hover:bg-blue-500/10 disabled:opacity-40"
              >
                <Play className="size-3" />
                恢复
              </button>
              <button
                type="button"
                data-testid="stop-run"
                disabled={!canStop}
                onClick={() => { const body = commandFor('stop_run'); if (body) void sendCommand(body); }}
                className="flex items-center gap-1 rounded px-2.5 py-1 text-xs font-medium text-rose-300 hover:bg-rose-500/10 disabled:opacity-40"
              >
                <Square className="size-3" />
                停止
              </button>
            </div>
          </div>
        </div>

        {terminationNotice && (
          <div role="status" data-testid="run-termination-notice" className="mt-2 rounded border border-amber-800/60 bg-amber-950/30 p-2 text-xs text-amber-100">
            <p className="font-medium">{terminationNotice.title}</p>
            <p className="mt-1 whitespace-pre-wrap break-words">{terminationNotice.detail}</p>
          </div>
        )}

        {/* Structured Advanced Controls Tray (Compact) */}
        <div className="mt-2 flex flex-wrap items-center gap-2 rounded-lg border border-gray-800/80 bg-gray-900/60 p-2 text-xs">
          <div className="flex flex-wrap items-center gap-1.5 border-r border-gray-800 pr-3">
            <span className="font-semibold text-gray-300">代次控制:</span>
            <label className="text-gray-400">原因
              <input
                data-testid="epoch-reason"
                placeholder="例如: 停止后继续探索"
                value={epochReason}
                onChange={(event) => setEpochReason(event.target.value)}
                className={`${fieldClass} ml-1`}
              />
            </label>
            <label className="text-gray-400">摘要
              <input
                aria-label="代次摘要"
                placeholder="可选摘要"
                value={epochSummary}
                onChange={(event) => setEpochSummary(event.target.value)}
                className={`${fieldClass} ml-1`}
              />
            </label>
            <button
              type="button"
              data-testid="start-epoch"
              disabled={!canEpoch || epochReason.trim() === ''}
              onClick={() => {
                const body = commandFor('start_epoch', { reason: epochReason.trim(), ...(epochSummary.trim() ? { summary: epochSummary.trim() } : {}) });
                if (body) void sendCommand(body);
              }}
              className="rounded bg-indigo-600 px-2 py-1 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-40"
            >
              继续任务（新 Epoch）
            </button>
          </div>

          <div className="flex flex-wrap items-center gap-1.5">
            <span className="font-semibold text-gray-300">追加授权:</span>
            {(() => {
              const flags = limitFlags(snap?.budget);
              if (flags.known && !flags.model && !flags.tool && !flags.seconds) {
                return <span data-testid="grant-unneeded" className="text-gray-400">无需追加授权</span>;
              }
              const model = flags.model ? grant.model : 0;
              const tool = flags.tool ? grant.tool : 0;
              const seconds = flags.seconds ? grant.seconds : 0;
              return (
                <>
                  {flags.model && <label className="text-gray-400">模型
                    <input
                      data-testid="grant-model-calls"
                      type="number"
                      value={grant.model}
                      onChange={(event) => setGrant((c) => ({ ...c, model: Number(event.target.value) }))}
                      className={`${fieldClass} ml-1 w-14`}
                    />
                  </label>}
                  {flags.tool && <label className="text-gray-400">工具
                    <input
                      aria-label="追加工具次数"
                      type="number"
                      value={grant.tool}
                      onChange={(event) => setGrant((c) => ({ ...c, tool: Number(event.target.value) }))}
                      className={`${fieldClass} ml-1 w-14`}
                    />
                  </label>}
                  {flags.seconds && <label className="text-gray-400">秒数
                    <input
                      aria-label="追加秒数"
                      type="number"
                      value={grant.seconds}
                      onChange={(event) => setGrant((c) => ({ ...c, seconds: Number(event.target.value) }))}
                      className={`${fieldClass} ml-1 w-14`}
                    />
                  </label>}
                  <button
                    type="button"
                    data-testid="grant-budget"
                    disabled={!canCommand || runStatus === 'completed' || runStatus === 'stopping' || (model <= 0 && tool <= 0 && seconds <= 0)}
                    onClick={() => {
                      const body = commandFor('grant_budget', {
                        additional_model_calls: model,
                        additional_tool_calls: tool,
                        additional_seconds: seconds,
                      });
                      if (body) void sendCommand(body);
                    }}
                    className="rounded bg-blue-600 px-2 py-1 text-xs font-medium text-white hover:bg-blue-500 disabled:opacity-40"
                  >
                    追加授权
                  </button>
                </>
              );
            })()}
          </div>
        </div>

        {/* Structured Metric Cards Row (Compact) */}
        {snap && (
          <div className="mt-2 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
            <div className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-1.5">
              <p className="text-[11px] text-gray-400">Worker 容量池</p>
              <p data-testid="worker-capacity" className="mt-0.5 font-mono text-[11px] text-gray-200">
                requested {snap.worker_capacity.requested} / effective {snap.worker_capacity.effective} / active {snap.worker_capacity.active} / queued {snap.worker_capacity.queued}
              </p>
              <p data-testid="worker-count" className="text-[10px] text-gray-500">方向 {snap.directions.length}</p>
            </div>

            <div className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-1.5">
              <p className="text-[11px] text-gray-400">模型并发与记录</p>
              <p data-testid="model-capacity" className="mt-0.5 font-mono text-[11px] text-gray-200">
                limit {snap.model_capacity.limit} / in_flight {snap.model_capacity.in_flight} / queued {snap.model_capacity.queued}
              </p>
              <p data-testid="model-call-count" className="text-[10px] text-gray-500">
                {modelCalls.error ? `读取失败：${modelCalls.error}` : `模型调用记录 ${modelCalls.count == null ? '未知' : modelCalls.count} 条`}
              </p>
            </div>

            <div className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-1.5">
              <p className="text-[11px] text-gray-400">源站控制</p>
              <p data-testid="origin-pacing" className="mt-0.5 font-mono text-[11px] text-gray-200">
                源站等待 {snap.origin_pacing?.waiting ? '是' : '否'}{snap.origin_pacing?.reason ? ` · ${snap.origin_pacing.reason}` : ''}
              </p>
            </div>

            <div className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-1.5">
              <p className="text-[11px] text-gray-400">全局预算配额</p>
              <p data-testid="budget-summary" className="mt-0.5 font-mono text-[10px] text-gray-300">
                {budgetSummaryText(snap.budget)}
              </p>
            </div>
          </div>
        )}

        {/* Alerts & feedback */}
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
      </header>}

      {/* Modern Workspace Tabs Bar */}
      {runId && !creating && (
        <div className="shrink-0 border-b border-gray-800 bg-gray-900/70 px-3 py-1.5 flex flex-wrap items-center justify-between gap-2 backdrop-blur">
          {/* Main workspace tab list */}
          <div role="tablist" aria-label="工作台主分区" className="flex flex-wrap items-center gap-1.5">
            <button
              type="button"
              role="tab"
              aria-selected={workbenchTab === 'main'}
              onClick={() => { setWorkbenchTab('main'); setActiveView('execute'); setPane('execute'); }}
              className={cn(
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all',
                workbenchTab === 'main'
                  ? 'bg-blue-600/20 text-blue-300 border border-blue-500/40 shadow-sm'
                  : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/60'
              )}
            >
              <MessageSquare className="size-3.5" />
              <span>会话协作 (Main Agent)</span>
              {messages.length > 0 && (
                <span className="rounded-full bg-blue-500/30 px-1.5 py-0.2 font-mono text-[10px] text-blue-300">
                  {messages.length}
                </span>
              )}
            </button>

            <button
              type="button"
              role="tab"
              aria-selected={workbenchTab === 'directions'}
              onClick={() => { setWorkbenchTab('directions'); setActiveView('execute'); setPane('execute'); }}
              className={cn(
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all',
                workbenchTab === 'directions'
                  ? 'bg-emerald-600/20 text-emerald-300 border border-emerald-500/40 shadow-sm'
                  : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/60'
              )}
            >
              <Layers className="size-3.5" />
              <span>Worker 并行方向</span>
              {snap && (
                <span className="rounded-full bg-emerald-500/30 px-1.5 py-0.2 font-mono text-[10px] text-emerald-300">
                  {snap.directions.length}
                </span>
              )}
            </button>

            <button
              type="button"
              role="tab"
              aria-selected={workbenchTab === 'activity'}
              onClick={() => { setWorkbenchTab('activity'); setActiveView('execute'); setPane('execute'); }}
              className={cn(
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all',
                workbenchTab === 'activity'
                  ? 'bg-amber-600/20 text-amber-300 border border-amber-500/40 shadow-sm'
                  : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/60'
              )}
            >
              <Activity className="size-3.5" />
              <span>活动流与 Planner 控制</span>
              {snap && (
                <span className="rounded-full bg-amber-500/30 px-1.5 py-0.2 font-mono text-[10px] text-amber-300">
                  {snap.events.length}
                </span>
              )}
            </button>

            <button
              type="button"
              role="tab"
              aria-selected={workbenchTab === 'inspector'}
              onClick={() => { setWorkbenchTab('inspector'); setPane('inspect'); }}
              className={cn(
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all',
                workbenchTab === 'inspector'
                  ? 'bg-purple-600/20 text-purple-300 border border-purple-500/40 shadow-sm'
                  : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/60'
              )}
            >
              <Sliders className="size-3.5" />
              <span>审计检查器 (Inspector)</span>
              {selection.workItemId && (
                <span className="rounded-full bg-purple-500/30 px-1.5 py-0.2 font-mono text-[10px] text-purple-300">
                  {selection.workItemId.slice(0, 6)}
                </span>
              )}
            </button>

            <button
              type="button"
              role="button"
              onClick={() => { setWorkbenchTab('graph'); setActiveView('graph'); setPane('execute'); }}
              className={cn(
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all',
                workbenchTab === 'graph'
                  ? 'bg-indigo-600/20 text-indigo-300 border border-indigo-500/40 shadow-sm'
                  : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/60'
              )}
            >
              <Network className="size-3.5" />
              <span>探索链路画板 (Graph)</span>
              {graphNodes.length > 0 && (
                <span className="rounded-full bg-indigo-500/30 px-1.5 py-0.2 font-mono text-[10px] text-indigo-300">
                  {graphNodes.length}
                </span>
              )}
            </button>

            <button
              type="button"
              role="tab"
              aria-selected={workbenchTab === 'resources'}
              onClick={() => { setWorkbenchTab('resources'); setPane('inspect'); }}
              className={cn(
                'flex items-center gap-1.5 rounded-md px-3 py-1 text-xs font-medium transition-all',
                workbenchTab === 'resources'
                  ? 'bg-cyan-600/20 text-cyan-300 border border-cyan-500/40 shadow-sm'
                  : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/60'
              )}
            >
              <Database className="size-3.5" />
              <span>全景能力与资源</span>
            </button>
          </div>

          {/* Mode switch & Sidebar toggle */}
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={() => setLayoutMode(layoutMode === 'tabs' ? 'split' : 'tabs')}
              className="flex items-center gap-1.5 rounded border border-gray-800 bg-gray-900/90 px-2.5 py-1 text-[11px] text-gray-300 hover:bg-gray-800 transition-colors"
              title="切换单标签卡沉浸模式与多栏并排模式"
            >
              {layoutMode === 'tabs' ? (
                <>
                  <Maximize2 className="size-3 text-blue-400" />
                  <span>📑 标签卡模式</span>
                </>
              ) : (
                <>
                  <LayoutGrid className="size-3 text-purple-400" />
                  <span>🪟 分栏并排模式</span>
                </>
              )}
            </button>

            <button
              type="button"
              onClick={() => setSidebarOpen(!sidebarOpen)}
              className="hidden lg:flex items-center gap-1.5 rounded border border-gray-800 bg-gray-900/90 px-2.5 py-1 text-[11px] text-gray-300 hover:bg-gray-800 transition-colors"
              title={sidebarOpen ? '收起任务侧边栏' : '展开任务侧边栏'}
            >
              {sidebarOpen ? <PanelLeftClose className="size-3" /> : <PanelLeftOpen className="size-3" />}
              <span>{sidebarOpen ? '收起侧栏' : '展开侧栏'}</span>
            </button>
          </div>
        </div>
      )}

      {/* Pane tabs for mobile */}
      <div role="tablist" aria-label="工作台分区" className="flex shrink-0 gap-1 border-b border-gray-800 px-2 py-1 lg:hidden">
        {([
          ['nav', '导航'],
          ['execute', '执行'],
          ['inspect', '检查'],
        ] as const).map(([id, label]) => (
          <button
            key={id}
            type="button"
            role="tab"
            aria-selected={pane === id}
            data-testid={`pane-${id}`}
            onClick={() => {
              setPane(id);
              if (id === 'inspect') setWorkbenchTab('inspector');
              else if (id === 'execute') setWorkbenchTab('main');
            }}
            className={`rounded px-2 py-1 text-xs ${pane === id ? 'bg-gray-800 text-white' : 'text-gray-400'}`}
          >
            {label}
          </button>
        ))}
      </div>

      {/* Main Workspace Body */}
      <div className="flex min-h-[20rem] flex-1 min-h-0">
        {/* Left Navigation */}
        <aside className={cn(
          'min-h-0 shrink-0 flex-col overflow-auto border-r border-gray-800 bg-gray-950',
          pane === 'nav' ? 'flex w-full' : 'hidden',
          sidebarOpen ? 'lg:flex lg:w-60' : 'lg:hidden'
        )}>
          <div className="flex items-center justify-between px-3 py-2 border-b border-gray-800/60">
            <h2 className="text-sm font-semibold">任务</h2>
            <button type="button" data-testid="open-create" onClick={() => { setCreating(true); setPane('execute'); }} className="text-xs text-blue-300 hover:underline">新任务</button>
          </div>
          {tasks.error && <p role="alert" className="px-3 text-xs text-red-200">{tasks.error}</p>}
          {tasks.data === null && !tasks.error && <p className="px-3 text-xs text-gray-400">正在读取任务</p>}
          {tasks.data && tasks.data.length === 0 && <p className="px-3 text-xs text-gray-400">没有任务。</p>}
          <ul className="space-y-1 p-2">
            {(tasks.data || []).map((task) => (
              <li key={task.id}>
                <Link
                  data-testid="task-link"
                  href={workbenchHref({ taskId: task.id })}
                  onClick={() => {
                    setCreating(false);
                    setPane('execute');
                  }}
                  className={`block rounded px-2.5 py-1.5 text-xs transition-colors ${task.id === taskId ? 'bg-gray-800 font-medium text-white' : 'text-gray-300 hover:bg-gray-900'}`}
                >
                  {task.name}
                </Link>
              </li>
            ))}
          </ul>
          <h2 className="px-3 pt-3 pb-1 text-sm font-semibold border-t border-gray-800/60">Run 运行记录</h2>
          {runs.error && <p role="alert" className="px-3 text-xs text-red-200">{runs.error}</p>}
          {taskId && runs.data === null && !runs.error && <p className="px-3 text-xs text-gray-400">正在读取运行</p>}
          <ul className="space-y-1 p-2">
            {(runs.data || []).map((run) => (
              <li key={run.id}>
                <Link
                  data-testid="run-link"
                  href={workbenchHref({ taskId: run.task_id, runId: run.id })}
                  onClick={() => {
                    setCreating(false);
                    setPane('execute');
                  }}
                  className={`block rounded px-2.5 py-1.5 text-xs transition-colors ${run.id === runId ? 'bg-gray-800 font-medium text-white' : 'text-gray-300 hover:bg-gray-900'}`}
                >
                  {run.id.slice(0, 8)} · 记录 {run.status}
                </Link>
              </li>
            ))}
          </ul>
          {snap && <p className="px-3 pt-2 text-xs text-gray-400">Planner {snap.planner.status}</p>}
          {runId && taskId && <Link data-testid="old-run-link" href={`/tasks/${taskId}/runs/${runId}`} className="px-3 py-2 text-xs text-blue-300 hover:underline">旧运行页</Link>}
          {runRecord.error && <p role="alert" className="px-3 text-xs text-red-200">{runRecord.error}</p>}
          {snapshot.status === 'error' && !snap && runRecord.data && <p className="px-3 text-xs text-amber-200">这是旧运行记录，不是 workbench.v1。统一命令已禁用。</p>}
        </aside>

        {/* Center Main Execution / Active View Area */}
        <section className={cn(
          'min-h-0 min-w-0 flex-1 flex-col overflow-hidden',
          pane === 'execute' || pane === 'inspect' ? 'flex' : 'hidden lg:flex'
        )}>
          {creating ? (
            <div data-testid="create-task-scroll" className="min-h-0 flex-1 overflow-y-auto p-3">
              <CreateTaskForm onCancel={() => setCreating(false)} />
            </div>
          ) : !runId ? (
            taskId ? (
              <div data-testid="task-overview" className="flex min-h-0 flex-1 flex-col overflow-y-auto p-4 space-y-4">
                {/* Task Header */}
                <div className="flex flex-wrap items-center justify-between gap-3 border-b border-gray-800 pb-3">
                  <div>
                    <div className="flex items-center gap-2">
                      <h2 className="text-base font-semibold text-white">
                        {taskRecord.data?.name || '任务详情'}
                      </h2>
                      <span className="font-mono text-xs text-gray-500">ID: {taskId.slice(0, 8)}</span>
                    </div>
                    <p className="mt-1 text-xs text-gray-400">
                      {taskRecord.data?.created_at ? `创建于 ${new Date(taskRecord.data.created_at).toLocaleString()}` : '正在加载任务元数据...'}
                    </p>
                  </div>
                  <div className="flex items-center gap-2">
                    <Link
                      href={`/tasks/${taskId}`}
                      className="rounded border border-gray-700 bg-gray-900 px-2.5 py-1 text-xs text-gray-300 hover:bg-gray-800"
                    >
                      旧任务管理页
                    </Link>
                    <button
                      type="button"
                      data-testid="start-new-run"
                      disabled={startingRun}
                      onClick={() => void handleStartRun()}
                      className="rounded bg-blue-600 px-3 py-1 text-xs font-medium text-white hover:bg-blue-500 disabled:opacity-40"
                    >
                      {startingRun ? '正在启动...' : '启动新运行'}
                    </button>
                  </div>
                </div>

                {startRunError && (
                  <div role="alert" className="rounded border border-red-500/30 bg-red-950/30 p-2.5 text-xs text-red-300">
                    启动运行失败：{startRunError}
                  </div>
                )}

                {/* Objective Card */}
                <div className="rounded-lg border border-gray-800 bg-gray-900/60 p-3">
                  <h3 className="text-xs font-semibold text-gray-300">任务目标 (Objective)</h3>
                  <p className="mt-1.5 text-xs text-gray-200 whitespace-pre-wrap leading-relaxed">
                    {taskRecord.data?.objective || (taskRecord.error ? `加载失败：${taskRecord.error}` : '正在读取目标...')}
                  </p>
                </div>

                {/* Scope & Config Cards */}
                <div className="grid grid-cols-1 md:grid-cols-2 gap-3 text-xs">
                  <div className="rounded-lg border border-gray-800 bg-gray-900/40 p-3">
                    <h3 className="font-semibold text-gray-300">目标范围 (Scope)</h3>
                    <pre className="mt-1 max-h-36 overflow-auto font-mono text-[11px] text-gray-300 whitespace-pre-wrap break-all">
                      {taskRecord.data?.scope ? JSON.stringify(taskRecord.data.scope, null, 2) : '加载中...'}
                    </pre>
                  </div>
                  <div className="rounded-lg border border-gray-800 bg-gray-900/40 p-3">
                    <h3 className="font-semibold text-gray-300">执行配置 (Configuration)</h3>
                    <pre className="mt-1 max-h-36 overflow-auto font-mono text-[11px] text-gray-300 whitespace-pre-wrap break-all">
                      {taskRecord.data?.config ? JSON.stringify(taskRecord.data.config, null, 2) : '加载中...'}
                    </pre>
                  </div>
                </div>

                {/* Runs List Card */}
                <div className="rounded-lg border border-gray-800 bg-gray-900/40 p-3">
                  <div className="flex items-center justify-between border-b border-gray-800/80 pb-2">
                    <h3 className="text-xs font-semibold text-gray-300">
                      执行运行记录 (Runs) {runs.data ? `(${runs.data.length})` : ''}
                    </h3>
                    {runs.data && runs.data.length > 0 && (
                      <span className="text-[11px] text-blue-400">正在进入最新运行现场...</span>
                    )}
                  </div>

                  {runs.error && (
                    <p role="alert" className="mt-2 text-xs text-red-300">运行记录读取失败：{runs.error}</p>
                  )}

                  {runs.data === null && !runs.error && (
                    <p className="mt-2 text-xs text-gray-400 animate-pulse">正在读取运行列表...</p>
                  )}

                  {runs.data && runs.data.length === 0 && (
                    <div className="py-4 text-center text-xs text-gray-400">
                      <p>此任务暂未创建执行运行。</p>
                      <p className="mt-1 text-gray-500">点击上方“启动新运行”即可开始自动化探索与分析。</p>
                    </div>
                  )}

                  {runs.data && runs.data.length > 0 && (
                    <div className="mt-2 space-y-1.5">
                      {runs.data.map((r) => (
                        <div
                          key={r.id}
                          className="flex items-center justify-between rounded border border-gray-800/80 bg-gray-950/60 px-3 py-2 text-xs"
                        >
                          <div className="flex items-center gap-2">
                            <span className={`inline-block size-2 rounded-full ${r.status === 'running' ? 'bg-emerald-400 animate-pulse' : r.status === 'paused' ? 'bg-amber-400' : 'bg-gray-400'}`} />
                            <span className="font-mono text-gray-200">{r.id.slice(0, 8)}</span>
                            <span className="rounded bg-gray-800 px-1.5 py-0.5 text-[10px] text-gray-300">状态 {r.status}</span>
                            <span className="font-mono text-[11px] text-gray-400">Epoch {r.current_epoch}</span>
                          </div>
                          <div className="flex items-center gap-3">
                            <span className="text-[11px] text-gray-500">{new Date(r.created_at).toLocaleString()}</span>
                            <Link
                              href={workbenchHref({ taskId: r.task_id, runId: r.id })}
                              onClick={() => {
                                setCreating(false);
                                setPane('execute');
                              }}
                              className="rounded bg-blue-600/20 px-2 py-0.5 text-[11px] font-medium text-blue-300 hover:bg-blue-600/30"
                            >
                              查看运行
                            </Link>
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              </div>
            ) : (
              <div className="p-4 text-sm text-gray-300">
                <p>选择任务和 Run，或创建新的探索任务。</p>
                <button type="button" onClick={() => setCreating(true)} className="mt-2 rounded bg-blue-600 px-3 py-1.5 text-xs">新任务</button>
              </div>
            )
          ) : (
            /* Run Workspace (Tabbed or Split) */
            <div className="flex min-h-0 flex-1 flex-col">
              {/* Conditional Views */}
              {workbenchTab === 'graph' ? (
                /* Tab: Exploration Canvas */
                <div className="relative min-h-0 flex-1">
                  <ExplorationCanvas
                    key={runId}
                    nodes={graphNodes}
                    edges={graphEdges}
                    notice={graphError ? `${graphError}${graph ? '；显示上次成功读取的数据' : ''}` : !graph ? '正在读取探索图…' : undefined}
                    hasError={Boolean(graphError)}
                    onOpenWork={openWork}
                  />
                </div>
              ) : workbenchTab === 'resources' ? (
                /* Tab: Full Capabilities & Resources */
                <div className="min-h-0 flex-1 overflow-auto p-4">
                  <ExplorationResources runId={runId} />
                </div>
              ) : (
                /* Run Workspace: Tabbed or Split */
                <div className={cn(
                  'flex min-h-0 flex-1',
                  layoutMode === 'split' ? 'flex-col lg:flex-row' : 'flex-col'
                )}>
                  {/* Left (or full-width) workspace area for Main / Directions / Activity */}
                  <div className={cn(
                    'flex min-h-0 flex-1 flex-col overflow-hidden',
                    ((workbenchTab === 'inspector' || (snapshot.status === 'error' && !snap && Boolean(runRecord.data))) && layoutMode === 'tabs') ||
                    (pane === 'inspect' && layoutMode !== 'split')
                      ? 'hidden'
                      : 'flex'
                  )}>
                    {/* Tab Panel 1: Main Agent Conversation */}
                    <div
                      role="tabpanel"
                      aria-label="Main Agent 会话协作"
                      className={cn(
                        'flex min-h-0 flex-1 flex-col overflow-hidden',
                        workbenchTab !== 'main' && 'hidden'
                      )}
                    >
                      {(taskRecord.error || (taskRecord.data && runRecord.data && gaps.length > 0)) && (
                        <div data-testid="execution-gaps" className="border-b border-gray-800 px-3 py-1.5 text-xs text-amber-200">
                          {taskRecord.error && <p role="alert">{taskRecord.error}</p>}
                          {gaps.length > 0 && <p>执行输入未保留：{gaps.join(', ')}。界面不会把这些字段显示成已经生效。</p>}
                        </div>
                      )}

                      {/* Main Conversation Stream - Full height, comfortable, spacious view */}
                      <div data-testid="main-conversation" className="min-h-0 flex-1 space-y-3 overflow-y-auto p-4" aria-live="polite">
                        <div className="flex items-center justify-between border-b border-gray-800/80 pb-2">
                          <div className="flex items-center gap-2">
                            <MessageSquare className="size-4 text-blue-400" />
                            <h2 className="text-sm font-semibold text-gray-200">Main Agent 战略对话与协同</h2>
                          </div>
                          <div className="flex items-center gap-3">
                            <span className="font-mono text-xs text-gray-500">{messages.length} 条会话消息</span>
                            <button
                              type="button"
                              onClick={() => { setWorkbenchTab('directions'); setActiveView('execute'); setPane('execute'); }}
                              className="text-xs text-emerald-400 hover:text-emerald-300 flex items-center gap-1"
                            >
                              <Layers className="size-3" />
                              Worker 并行方向 ({snap?.directions.length || 0}) →
                            </button>
                            <button
                              type="button"
                              onClick={() => { setWorkbenchTab('activity'); setActiveView('execute'); setPane('execute'); }}
                              className="text-xs text-amber-400 hover:text-amber-300 flex items-center gap-1"
                            >
                              <Activity className="size-3" />
                              活动事件与 Planner →
                            </button>
                          </div>
                        </div>

                        {messageError && <p role="alert" className="text-xs text-red-200">{messageError.message}</p>}

                        {messages.map((message) => {
                          const isUser = message.role === 'user';
                          return (
                            <article
                              key={message.seq}
                              data-testid="message-row"
                              data-command-id={message.command_id || undefined}
                              className={cn(
                                'rounded-xl border p-3.5 text-xs leading-relaxed transition-shadow',
                                isUser
                                  ? 'border-blue-500/40 bg-blue-950/20 ml-12 shadow-sm'
                                  : 'border-gray-800 bg-gray-900/60 mr-12 shadow-sm'
                              )}
                            >
                              <div className="flex items-center justify-between text-[11px] text-gray-400 pb-1.5 border-b border-gray-800/40">
                                <span className={cn('font-semibold flex items-center gap-1.5', isUser ? 'text-blue-300' : 'text-gray-300')}>
                                  {message.role === 'user' ? '👤 用户 (Operator)' : message.role === 'assistant' ? '🤖 Main Agent' : message.role} · seq {message.seq}{message.turn_status ? ` · ${message.turn_status}` : ''}
                                </span>
                              </div>
                              {message.text && (
                                <p className="mt-2 text-gray-200 whitespace-pre-wrap leading-relaxed text-[13px]">{message.text}</p>
                              )}
                              <p className="mt-2.5 font-mono text-[10px] text-gray-500 pt-1.5 border-t border-gray-800/30">
                                {message.command_id ? `command ${message.command_id}` : ''}
                                {message.model_call_id ? ` · model_call ${message.model_call_id}` : ''}
                                {message.tool_call_id ? ` · tool_call ${message.tool_call_id}` : ''}
                                {message.work_item_id ? ` · work_item ${message.work_item_id}` : ''}
                              </p>
                            </article>
                          );
                        })}

                        {messages.length === 0 && !messageError && (
                          <div className="py-16 text-center text-xs text-gray-500">
                            <MessageSquare className="size-8 text-gray-600 mx-auto mb-2 opacity-50" />
                            <p className="text-sm font-medium text-gray-400">暂无主对话消息</p>
                            <p className="mt-1 text-gray-600">在下方输入指令指导 Main Agent 或调整全局探索策略。</p>
                          </div>
                        )}
                      </div>

                      {/* Submission form */}
                      <form
                        className="shrink-0 border-t border-gray-800 bg-gray-950 p-3 sm:p-4 shadow-lg"
                        onSubmit={(event) => {
                          event.preventDefault();
                          if (!snap || !draft.trim() || submission.phase === 'pending') return;
                          void submitMessage(idemKey, { message: draft.trim(), expected_epoch: snap.run.epoch_number });
                        }}
                      >
                        <label className="block text-xs font-medium text-gray-300">
                          给 Main Agent 的消息
                          <textarea
                            aria-label="给 Main Agent 的消息"
                            placeholder="输入指令指导 Main Agent 或调整探索策略... (按 Ctrl+Enter 快捷发送)"
                            value={draft}
                            onChange={(event) => { setDraft(event.target.value); setIdemKey(newRequestID()); }}
                            onKeyDown={(event) => {
                              if ((event.ctrlKey || event.metaKey) && event.key === 'Enter') {
                                event.preventDefault();
                                if (snap && draft.trim() && submission.phase !== 'pending') {
                                  void submitMessage(idemKey, { message: draft.trim(), expected_epoch: snap.run.epoch_number });
                                }
                              }
                            }}
                            rows={3}
                            className={`${fieldClass} mt-1.5 w-full resize-y text-xs`}
                          />
                        </label>

                        <div className="mt-2.5 flex flex-wrap items-center justify-between gap-2">
                          <div className="flex flex-wrap items-center gap-2">
                            <button
                              type="submit"
                              data-testid="send-message"
                              disabled={!snap || !draft.trim() || submission.phase === 'pending'}
                              className="flex items-center gap-1.5 rounded-lg bg-blue-600 px-4 py-1.5 text-xs font-medium text-white hover:bg-blue-500 disabled:opacity-40 transition-colors shadow-sm"
                            >
                              <Send className="size-3" />
                              发送
                            </button>
                            {submission.canResend && lastSubmit && (
                              <button
                                type="button"
                                data-testid="resend-message"
                                onClick={() => void submitMessage(lastSubmit.key, lastSubmit.body)}
                                className="text-xs text-blue-300 hover:underline"
                              >
                                重发上一请求
                              </button>
                            )}
                            {lastSubmit && draft.trim() && draft.trim() !== lastSubmit.body.message && (
                              <button
                                type="button"
                                data-testid="conflict-resend"
                                onClick={() => void submitMessage(lastSubmit.key, { message: draft.trim(), expected_epoch: lastSubmit.body.expected_epoch })}
                                className="text-xs text-amber-200 hover:underline"
                              >
                                用上一把钥匙提交当前草稿
                              </button>
                            )}
                          </div>

                          <p role="status" data-testid="submission-status" className="text-xs text-gray-400">
                            {submission.message}
                            {submission.httpStatus != null ? ` · HTTP ${submission.httpStatus}` : ''}
                            {submission.messageId ? ` · message ${submission.messageId}` : ''}
                            {submission.turnId ? ` · turn ${submission.turnId}` : ''}
                            {submission.sessionId ? ` · session ${submission.sessionId}` : ''}
                            {submission.key ? ` · key ${submission.key}` : ''}
                          </p>
                        </div>
                      </form>
                    </div>

                    {/* Tab Panel 2: Worker 并行方向 Fleet Grid */}
                    <div
                      role="tabpanel"
                      aria-label="Worker 并行方向"
                      className={cn(
                        'min-h-0 flex-1 overflow-y-auto p-4',
                        workbenchTab !== 'directions' && 'hidden'
                      )}
                    >
                      <div className="flex items-center justify-between pb-3 border-b border-gray-800 mb-4">
                        <div>
                          <div className="flex items-center gap-2">
                            <Layers className="size-4 text-emerald-400" />
                            <h2 className="text-sm font-semibold text-white">Worker 并行方向集群 (Worker Fleet)</h2>
                          </div>
                          <p className="text-xs text-gray-400 mt-0.5">
                            查看各 Worker 探索方向的实时状态、最新步骤与局部额度审计。点击卡片可深入单方向检查器。
                          </p>
                        </div>
                        <div className="flex items-center gap-2">
                          <span className="rounded-full bg-emerald-500/20 text-emerald-300 px-3 py-1 text-xs font-mono font-medium">
                            方向总数: {snap?.directions.length || 0}
                          </span>
                          <button
                            type="button"
                            onClick={() => { setWorkbenchTab('graph'); setActiveView('graph'); }}
                            className="rounded border border-gray-700 bg-gray-900 px-2.5 py-1 text-xs text-gray-300 hover:bg-gray-800 flex items-center gap-1"
                          >
                            <Network className="size-3 text-indigo-400" />
                            查看探索链路画板 →
                          </button>
                        </div>
                      </div>

                      <div
                        data-testid="direction-board"
                        className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4"
                      >
                        {snapshot.status === 'loading' && !snap && (
                          <p className="text-xs text-gray-400 col-span-full py-8 text-center">正在读取工作台快照...</p>
                        )}
                        {snap && snap.directions.length === 0 && (
                          <p data-testid="direction-empty" className="text-xs text-gray-400 col-span-full py-8 text-center">当前快照没有方向。</p>
                        )}
                        {snap?.directions.map((item) => (
                          <article
                            key={item.work_item_id}
                            data-testid="direction-card"
                            data-work-item-id={item.work_item_id}
                            data-status={item.status}
                            className={cn(
                              'group relative rounded-xl border p-3.5 text-xs transition-all flex flex-col justify-between shadow-sm',
                              item.work_item_id === selection.workItemId
                                ? 'border-emerald-500 bg-emerald-950/20 shadow-md ring-1 ring-emerald-500/30'
                                : 'border-gray-800 bg-gray-900/60 hover:border-gray-700 hover:bg-gray-900/90'
                            )}
                          >
                            <span data-testid={`direction-card-${item.work_item_id}`} className="sr-only">{item.work_item_id}</span>
                            <div>
                              <div className="flex items-start justify-between gap-2">
                                <button
                                  type="button"
                                  onClick={() => openWork(item.work_item_id)}
                                  className="text-left font-semibold text-white group-hover:text-emerald-300 line-clamp-2 focus:outline-none"
                                  title={item.objective || '未提供目标'}
                                >
                                  {item.objective || '未提供目标'}
                                </button>
                                <span
                                  className={cn(
                                    'shrink-0 rounded px-2 py-0.5 text-[10px] font-medium font-mono',
                                    item.status === 'running'
                                      ? 'bg-blue-500/20 text-blue-300 animate-pulse'
                                      : item.status === 'completed' || item.status === 'done'
                                      ? 'bg-emerald-500/20 text-emerald-300'
                                      : item.status === 'killed' || item.status === 'failed'
                                      ? 'bg-rose-500/20 text-rose-300'
                                      : 'bg-gray-800 text-gray-400'
                                  )}
                                >
                                  状态 {item.status}
                                </span>
                              </div>

                              <div className="mt-2.5 space-y-1.5 text-gray-400 text-[11px]">
                                <div className="flex items-center gap-1.5">
                                  <span className="text-gray-500">Attempt:</span>
                                  <span className="font-mono text-gray-300 truncate">{item.attempt_id || '无'}</span>
                                </div>
                                <div className="flex items-center gap-1.5">
                                  <span className="text-gray-500">最新步骤:</span>
                                  <strong className="text-gray-200 font-normal truncate">{item.latest_step || '未提供'}</strong>
                                </div>
                                <div className="flex items-center gap-1.5">
                                  <span className="text-gray-500">来源:</span>
                                  <span className="truncate">{item.source_refs?.length ? item.source_refs.join(', ') : '未提供'}</span>
                                </div>
                                <div className="font-mono text-[10px] text-gray-500 bg-gray-950/60 rounded p-1.5 border border-gray-800/60 break-all">
                                  局部额度: {item.local_budget ? JSON.stringify(item.local_budget) : '未提供'}
                                </div>
                                {item.error && (
                                  <p className="mt-1 text-[11px] text-rose-300 rounded bg-rose-950/30 p-1.5 border border-rose-800/40">
                                    失败 {item.error.code}: {item.error.message}
                                  </p>
                                )}
                              </div>
                            </div>

                            <div className="mt-3 pt-2.5 border-t border-gray-800/80 flex items-center justify-between">
                              <span className="font-mono text-[10px] text-gray-500 truncate">{item.work_item_id.slice(0, 8)}...</span>
                              <button
                                type="button"
                                onClick={() => openWork(item.work_item_id)}
                                className="rounded bg-emerald-600/20 px-2.5 py-1 text-xs font-medium text-emerald-300 hover:bg-emerald-600/30 transition-colors"
                              >
                                进入详细检查器 →
                              </button>
                            </div>
                          </article>
                        ))}
                      </div>
                    </div>

                    {/* Tab Panel 3: 活动事件流与 Planner 控制 */}
                    <div
                      role="tabpanel"
                      aria-label="活动事件流与 Planner 控制"
                      className={cn(
                        'min-h-0 flex-1 overflow-y-auto p-4',
                        workbenchTab !== 'activity' && 'hidden'
                      )}
                    >
                      <div className="flex items-center justify-between pb-3 border-b border-gray-800 mb-4">
                        <div>
                          <div className="flex items-center gap-2">
                            <Activity className="size-4 text-amber-400" />
                            <h2 className="text-sm font-semibold text-white">活动事件流与 Planner 控制 (Activity & Planner)</h2>
                          </div>
                          <p className="text-xs text-gray-400 mt-0.5">
                            实时追踪完整事件序列、监控 Planner 状态并注入指导提示或追加探索意图。
                          </p>
                        </div>
                        <div className="flex items-center gap-2">
                          <span className="rounded-full bg-amber-500/20 text-amber-300 px-3 py-1 text-xs font-mono font-medium">
                            事件数: {snap?.events.length || 0}
                          </span>
                          {snap?.planner?.status && (
                            <span className="rounded-full bg-blue-500/20 text-blue-300 px-2.5 py-0.5 text-xs font-mono">
                              Planner: {snap.planner.status}
                            </span>
                          )}
                        </div>
                      </div>

                      <div className="grid grid-cols-1 lg:grid-cols-3 gap-4">
                        {/* Event Timeline */}
                        <div className="lg:col-span-2 flex flex-col rounded-xl border border-gray-800 bg-gray-900/40 p-3">
                          <div className="flex items-center justify-between pb-2 border-b border-gray-800/80 mb-2">
                            <span className="text-xs font-semibold text-gray-300">事件序列 (Event Timeline)</span>
                            {snap?.has_more_events && <span className="text-[11px] text-amber-400">事件页未读完...</span>}
                          </div>

                          <div
                            data-testid="activity-stream"
                            className="space-y-1.5 overflow-y-auto max-h-[640px] pr-1"
                          >
                            <h2 className="sr-only">活动</h2>
                            {snap?.events.map((event) => (
                              <div
                                key={event.seq}
                                className="flex items-start gap-2 rounded-lg border border-gray-800/60 bg-gray-950/70 p-2 text-xs hover:border-gray-700 transition-colors"
                              >
                                <span className="shrink-0 rounded bg-blue-950/80 border border-blue-800/60 px-1.5 py-0.5 font-mono text-[10px] text-blue-300 font-bold">
                                  #{event.seq}
                                </span>
                                <span className="shrink-0 rounded bg-gray-800/90 px-1.5 py-0.5 font-mono text-[10px] text-gray-200">
                                  {event.type}
                                </span>
                                <div className="min-w-0 flex-1">
                                  <p className="font-mono text-[11px] text-gray-300 break-all">
                                    {event.payload ? JSON.stringify(event.payload) : ''}
                                  </p>
                                </div>
                              </div>
                            ))}

                            {snap && snap.events.length === 0 && (
                              <div className="py-12 text-center text-xs text-gray-500">
                                快照暂无事件记录。
                              </div>
                            )}
                          </div>
                        </div>

                        {/* Planner Status & Controls */}
                        <div className="space-y-4">
                          {/* Planner Status Card */}
                          <div className="rounded-xl border border-gray-800 bg-gray-900/40 p-3">
                            <h3 className="text-xs font-semibold text-gray-300 flex items-center gap-1.5">
                              <Brain className="size-3.5 text-purple-400" />
                              <span>Planner 态势监视</span>
                            </h3>
                            <div className="mt-2 space-y-2 text-xs">
                              <div className="flex items-center justify-between rounded bg-gray-950/60 p-2 border border-gray-800/60">
                                <span className="text-gray-400">调度器状态</span>
                                <span className="font-mono font-medium text-blue-300">{snap?.planner?.status || '未知'}</span>
                              </div>
                              {snap?.planner?.reason && (
                                <div className="rounded bg-gray-950/60 p-2 border border-gray-800/60">
                                  <span className="text-gray-400 block text-[11px]">当前原因 (Reason)</span>
                                  <span className="font-mono text-gray-200 mt-0.5 block break-all">
                                    {snap.planner.reason}
                                  </span>
                                </div>
                              )}
                              {snap?.planner?.session_id && (
                                <div className="rounded bg-gray-950/60 p-2 border border-gray-800/60">
                                  <span className="text-gray-400 block text-[11px]">Planner 会话 (Session)</span>
                                  <span className="font-mono text-gray-400 mt-0.5 block break-all">
                                    {snap.planner.session_id}
                                  </span>
                                </div>
                              )}
                            </div>
                          </div>

                          {/* Planner Controls Card */}
                          <div className="rounded-xl border border-gray-800 bg-gray-900/40 p-3 space-y-3">
                            <div>
                              <h3 className="text-xs font-semibold text-gray-300">Planner 指导与意图控制</h3>
                              <p className="text-[11px] text-gray-400 mt-0.5">
                                在线纠偏 Planner 的探索路径，或安排全新的目标意图。
                              </p>
                            </div>

                            {/* Hint input */}
                            <div className="rounded-lg border border-gray-800/80 bg-gray-950/50 p-2.5">
                              <label className="block text-xs font-medium text-gray-300">
                                注入提示 (Hint)
                                <span className="block text-[10px] text-gray-500 font-normal mt-0.5">给 Planner 注入指引信息（如重点关注子路径）</span>
                                <input
                                  aria-label="hint"
                                  value={hint}
                                  onChange={(event) => setHint(event.target.value)}
                                  placeholder="例如：优先检查管理后台入口与未授权接口..."
                                  className={`${fieldClass} mt-1.5 w-full text-xs`}
                                />
                              </label>
                              <div className="mt-2 flex justify-end">
                                <button
                                  type="button"
                                  disabled={!canCommand || !hint.trim()}
                                  onClick={() => {
                                    const body = commandFor('hint', { text: hint.trim() });
                                    if (body) void sendCommand(body);
                                  }}
                                  className="rounded bg-blue-600/80 px-3 py-1 text-xs font-medium text-white hover:bg-blue-600 disabled:opacity-40 transition-colors"
                                >
                                  提交 Hint
                                </button>
                              </div>
                            </div>

                            {/* Add intent input */}
                            <div className="rounded-lg border border-gray-800/80 bg-gray-950/50 p-2.5">
                              <label className="block text-xs font-medium text-gray-300">
                                追加意图 (Add Intent)
                                <span className="block text-[10px] text-gray-500 font-normal mt-0.5">为后续规划安排新探索目标</span>
                                <input
                                  aria-label="add_intent"
                                  value={intent}
                                  onChange={(event) => setIntent(event.target.value)}
                                  placeholder="例如：对所发现的 8080 端口服务进行深度指纹识别..."
                                  className={`${fieldClass} mt-1.5 w-full text-xs`}
                                />
                              </label>
                              <div className="mt-2 flex justify-end">
                                <button
                                  type="button"
                                  disabled={!canCommand || !intent.trim()}
                                  onClick={() => {
                                    const body = commandFor('add_intent', { summary: intent.trim() });
                                    if (body) void sendCommand(body);
                                  }}
                                  className="rounded bg-emerald-600/80 px-3 py-1 text-xs font-medium text-white hover:bg-emerald-600 disabled:opacity-40 transition-colors"
                                >
                                  提交意图
                                </button>
                              </div>
                            </div>
                          </div>
                        </div>
                      </div>
                    </div>
                  </div>

                  {/* Right (or full-width) Inspector Panel */}
                  {((workbenchTab === 'inspector' || (snapshot.status === 'error' && !snap && Boolean(runRecord.data))) || layoutMode === 'split' || pane === 'inspect') && (
                    <aside className={cn(
                      'min-h-0 flex-col overflow-hidden bg-gray-950',
                      (workbenchTab === 'inspector' || (snapshot.status === 'error' && !snap && Boolean(runRecord.data))) && layoutMode === 'tabs'
                        ? 'flex flex-1'
                        : layoutMode === 'split'
                        ? 'hidden lg:flex lg:w-[460px] xl:w-[540px] border-l border-gray-800'
                        : pane === 'inspect'
                        ? 'flex w-full'
                        : 'hidden'
                    )}>
                      {/* Worker directions quick switcher in Inspector */}
                      {snap && snap.directions.length > 0 && (
                        <div className="shrink-0 border-b border-gray-800 bg-gray-900/60 px-3 py-2">
                          <div className="flex items-center justify-between pb-1.5">
                            <span className="text-[11px] font-semibold text-gray-400">选择 Worker 方向切换审计:</span>
                            <span className="font-mono text-[10px] text-gray-500">共 {snap.directions.length} 个方向</span>
                          </div>
                          <div className="flex flex-wrap gap-2 overflow-x-auto pb-1">
                            {snap.directions.map((d) => (
                              <button
                                key={d.work_item_id}
                                type="button"
                                onClick={() => openWork(d.work_item_id)}
                                className={cn(
                                  'flex items-center gap-2 rounded-lg border px-2.5 py-1 text-xs transition-all',
                                  d.work_item_id === selection.workItemId
                                    ? 'border-purple-500 bg-purple-950/40 text-purple-200 ring-1 ring-purple-500/50 font-medium'
                                    : 'border-gray-800 bg-gray-900/60 text-gray-300 hover:border-gray-700'
                                )}
                              >
                                <span className="truncate max-w-[160px]">{d.objective || d.work_item_id.slice(0, 8)}</span>
                                <span className="font-mono text-[10px] text-gray-400">状态 {d.status}</span>
                              </button>
                            ))}
                          </div>
                        </div>
                      )}

                      <div className="min-h-0 flex-1 flex flex-col overflow-hidden">
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
                      </div>
                    </aside>
                  )}
                </div>
              )}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}
