'use client';

import React, { useEffect, useState, useCallback, useRef } from 'react';
import { useParams } from 'next/navigation';
import Link from 'next/link';
import { api, newRequestID } from '@/lib/api';
import { Run, RunEvent, AgentMessage, ModelCall, AgentToolCall, WorkItem, Observation, AgentSession, PlannerRound, Asset, WorkItemDetail, AssetDiscoveryDetail, Goal, ExplorationNode, ExecutionEpoch, BudgetGrant, EffectiveBudget } from '@/lib/types';
import { RunStatusBadge } from '@/components/RunStatusBadge';
import { ExplorationResources } from '@/components/ExplorationResources';
import { CheckResultsPanel } from '@/components/CheckResultsPanel';
import { EventTimeline } from '@/components/EventTimeline';

interface ParsedHttpEndpointOutput {
  request?: {
    method?: string;
    url?: string;
    asset_id?: string;
    started_at?: string;
  };
  response?: {
    status_code?: number;
    headers?: Record<string, string>;
    body?: string;
    received_at?: string;
    duration_ms?: number;
  };
  integrity?: {
    saved_bytes?: number;
    is_truncated?: boolean;
    note?: string;
  };
  error?: {
    type?: string;
    message?: string;
  };
}

function parseHttpOutput(output?: string): ParsedHttpEndpointOutput | null {
  if (!output) return null;
  try {
    const data = JSON.parse(output);
    if (data && typeof data === 'object' && (data.request || data.response || data.integrity || data.error)) {
      return data;
    }
    return null;
  } catch {
    return null;
  }
}

function getStatusCodeBadge(code?: number) {
  if (!code) return null;
  let color = 'bg-gray-800 text-gray-300 border-gray-700';
  if (code >= 200 && code < 300) {
    color = 'bg-emerald-500/10 text-emerald-400 border-emerald-500/30';
  } else if (code >= 300 && code < 400) {
    color = 'bg-amber-500/10 text-amber-400 border-amber-500/30';
  } else if (code >= 400) {
    color = 'bg-red-500/10 text-red-400 border-red-500/30';
  }
  return (
    <span className={`px-2 py-0.5 rounded text-xs font-mono font-bold border ${color}`}>
      {code}
    </span>
  );
}

export default function RunDetailPage() {
  const params = useParams();
  const taskId = params.taskId as string;
  const runId = params.runId as string;

  const [run, setRun] = useState<Run | null>(null);
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [modelCalls, setModelCalls] = useState<ModelCall[]>([]);
  const [toolCalls, setToolCalls] = useState<AgentToolCall[]>([]);
  const [workItems, setWorkItems] = useState<WorkItem[]>([]);
  const [workItemDetails, setWorkItemDetails] = useState<WorkItemDetail[]>([]);
  const [observations, setObservations] = useState<Observation[]>([]);
  const [sessions, setSessions] = useState<AgentSession[]>([]);
  const [plannerRounds, setPlannerRounds] = useState<PlannerRound[]>([]);
  const [runAssets, setRunAssets] = useState<Asset[]>([]);
  const [discoveries, setDiscoveries] = useState<AssetDiscoveryDetail[]>([]);
  const [goals, setGoals] = useState<Goal[]>([]);
  const [graphNodes, setGraphNodes] = useState<ExplorationNode[]>([]);
  const [epochs, setEpochs] = useState<ExecutionEpoch[]>([]);
  const [budgetGrants, setBudgetGrants] = useState<BudgetGrant[]>([]);
  const [effectiveBudget, setEffectiveBudget] = useState<EffectiveBudget | null>(null);

  const [steerTarget, setSteerTarget] = useState<{ id: string; title: string; instruction: string } | null>(null);
  const [grantFormOpen, setGrantFormOpen] = useState(false);
  const [controlPending, setControlPending] = useState(false);
  const controlLock = useRef(false);
  const steerRequest = useRef<{ signature: string; key: string } | null>(null);
  const grantRequest = useRef<{ signature: string; key: string } | null>(null);
  const beginControl = () => {
    if (controlLock.current) return false;
    controlLock.current = true; setControlPending(true); return true;
  };
  const endControl = () => { controlLock.current = false; setControlPending(false); };
  const [grantModelCalls, setGrantModelCalls] = useState(10);
  const [grantToolCalls, setGrantToolCalls] = useState(20);
  const [grantSeconds, setGrantSeconds] = useState(60);
  const [grantReason, setGrantReason] = useState('人工授权追加额度');
  const [newEpochReason, setNewEpochReason] = useState('操作员手动推进新执行代次');

  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState<string | null>(null);
  const [configOpen, setConfigOpen] = useState(false);
  const [activeTab, setActiveTab] = useState<'work_items' | 'goals_graph' | 'control_budget' | 'discoveries' | 'observations' | 'sessions' | 'timeline' | 'model_calls' | 'tool_calls' | 'messages'>('goals_graph');

  const loadRunData = useCallback(async (isInitial = false) => {
    if (isInitial) setLoading(true);
    setError(null);
    try {
      const [
        runData,
        eventsData,
        messagesData,
        modelCallsData,
        toolCallsData,
        workItemsData,
        workItemDetailsData,
        observationsData,
        sessionsData,
        plannerRoundsData,
        runAssetsData,
        discoveriesData,
        goalsData,
        graphData,
        epochsData,
        grantsData,
        budgetData,
      ] = await Promise.all([
        api.getRun(runId),
        api.getRunEvents(runId),
        api.getRunMessages(runId).catch(() => []),
        api.getRunModelCalls(runId).catch(() => []),
        api.getRunToolCalls(runId).catch(() => []),
        api.getRunWorkItems(runId).catch(() => []),
        api.getRunWorkItemDetails(runId).catch(() => []),
        api.getRunObservations(runId).catch(() => []),
        api.getRunSessions(runId).catch(() => []),
        api.getRunPlannerRounds(runId).catch(() => []),
        api.getRunAssets(runId).catch(() => []),
        api.getRunDiscoveries(runId).catch(() => []),
        api.getGoals(runId).catch(() => []),
        api.getGraphOverview(runId).catch(() => ({ nodes: [], total_nodes: 0 })),
        api.listEpochs(runId).catch(() => []),
        api.listBudgetGrants(runId).catch(() => []),
        api.getEffectiveBudget(runId).catch(() => null),
      ]);
      setRun(runData);
      setEvents(eventsData);
      setMessages(messagesData);
      setModelCalls(modelCallsData);
      setToolCalls(toolCallsData);
      setWorkItems(workItemsData);
      setWorkItemDetails(workItemDetailsData);
      setObservations(observationsData);
      setSessions(sessionsData);
      setPlannerRounds(plannerRoundsData);
      setRunAssets(runAssetsData);
      setDiscoveries(discoveriesData);
      setGoals(goalsData);
      setGraphNodes(graphData.nodes || []);
      setEpochs(epochsData);
      setBudgetGrants(grantsData);
      setEffectiveBudget(budgetData);

      // Default active tab selection
      if (isInitial) {
        if (goalsData.length > 0 || (graphData.nodes && graphData.nodes.length > 0)) {
          setActiveTab('goals_graph');
        } else if (discoveriesData.length > 0 || (runData?.execution_input as any)?.discovery_policy) {
          setActiveTab('discoveries');
        } else if (workItemsData.length > 0) {
          setActiveTab('work_items');
        } else {
          setActiveTab('timeline');
        }
      }
    } catch (err: unknown) {
      if (err instanceof Error) {
        setError(err.message);
      } else {
        setError('Failed to load run details');
      }
    } finally {
      if (isInitial) setLoading(false);
    }
  }, [runId]);

  useEffect(() => {
    loadRunData(true);
  }, [loadRunData]);

  // Refresh the complete projection, including goals, graph and epochs. A
  // terminal status must not leave the other panels at their previous state.
  const isPollingRef = useRef(false);
  useEffect(() => {
    if (!run || !['queued', 'running', 'paused', 'stopping'].includes(run.status.toLowerCase())) return;
    const interval = setInterval(async () => {
      if (isPollingRef.current) return;
      isPollingRef.current = true;
      try { await loadRunData(); }
      finally { isPollingRef.current = false; }
    }, 2000);
    return () => clearInterval(interval);
  }, [run?.status, loadRunData]);

  const handleStopRun = async () => {
    if (!run || !['queued', 'running', 'paused'].includes(run.status) || !beginControl()) return;

    setStopping(true);
    setStopError(null);
    try {
      const updated = await api.stopRun(runId);
      setRun(updated);
      const updatedEvents = await api.getRunEvents(runId);
      setEvents(updatedEvents);
    } catch (err: unknown) {
      if (err instanceof Error) {
        setStopError(err.message);
      } else {
        setStopError('Failed to stop run');
      }
    } finally {
      setStopping(false);
      endControl();
    }
  };

  const handlePause = async () => {
    if (!beginControl()) return;
    try {
      await api.pauseRun(runId);
      await loadRunData();
    } catch (e: any) {
      alert('暂停失败: ' + (e.message || e));
    } finally { endControl(); }
  };

  const handleResume = async () => {
    if (!beginControl()) return;
    try {
      await api.resumeRun(runId);
      await loadRunData();
    } catch (e: any) {
      alert('恢复失败: ' + (e.message || e));
    } finally { endControl(); }
  };

  const handleSteer = async () => {
    if (!steerTarget || !beginControl()) return;
    try {
      const signature = JSON.stringify([runId, steerTarget.id, steerTarget.instruction]);
      if (steerRequest.current?.signature !== signature) steerRequest.current = { signature, key: newRequestID() };
      await api.steerWorkItem(runId, steerTarget.id, steerTarget.instruction, undefined, steerRequest.current.key);
      steerRequest.current = null;
      setSteerTarget(null);
      await loadRunData();
    } catch (e: any) {
      alert('纠偏失败: ' + (e.message || e));
    } finally { endControl(); }
  };

  const handleKill = async (wiId: string) => {
    if (!confirm('确定要终止该工作项吗？') || !beginControl()) return;
    try {
      await api.killWorkItem(runId, wiId, 'killed_by_operator');
      await loadRunData();
    } catch (e: any) {
      alert('终止工作项失败: ' + (e.message || e));
    } finally { endControl(); }
  };

  const handleGrantBudget = async () => {
    if (!beginControl()) return;
    try {
      const signature = JSON.stringify([runId, grantModelCalls, grantToolCalls, grantSeconds, grantReason]);
      if (grantRequest.current?.signature !== signature) grantRequest.current = { signature, key: newRequestID() };
      await api.grantBudget(runId, {
        idempotency_key: grantRequest.current.key,
        additional_model_calls: grantModelCalls,
        additional_tool_calls: grantToolCalls,
        additional_seconds: grantSeconds,
        reason: grantReason,
      });
      grantRequest.current = null;
      setGrantFormOpen(false);
      await loadRunData();
    } catch (e: any) {
      alert('追加预算失败: ' + (e.message || e));
    } finally { endControl(); }
  };

  const handleStartEpoch = async () => {
    if (!beginControl()) return;
    try {
      await api.startNewEpoch(runId, newEpochReason, '操作员手动推进', run?.current_epoch);
      await loadRunData();
    } catch (e: any) {
      alert('推进代次失败: ' + (e.message || e));
    } finally { endControl(); }
  };

  const calculateDuration = (): string => {
    if (!run) return '-';
    const start = run.started_at
      ? new Date(run.started_at).getTime()
      : run.created_at
      ? new Date(run.created_at).getTime()
      : null;

    if (!start) return '-';

    if (run.finished_at) {
      const finish = new Date(run.finished_at).getTime();
      const diffSec = Math.max(0, Math.round((finish - start) / 1000));
      if (diffSec < 60) return `${diffSec}s`;
      const mins = Math.floor(diffSec / 60);
      const secs = diffSec % 60;
      return `${mins}m ${secs}s`;
    }

    if (run.status === 'running') {
      const now = Date.now();
      const diffSec = Math.max(0, Math.round((now - start) / 1000));
      return `${diffSec}s (running)`;
    }

    return '-';
  };

  if (loading) {
    return (
      <div className="space-y-6 animate-pulse">
        <div className="h-6 w-32 bg-gray-800 rounded" />
        <div className="h-28 w-full bg-gray-900 rounded-2xl" />
        <div className="h-44 w-full bg-gray-900 rounded-xl" />
        <div className="h-96 w-full bg-gray-900 rounded-xl" />
      </div>
    );
  }

  if (error || !run) {
    return (
      <div className="space-y-4">
        <Link
          href={`/tasks/${taskId}`}
          className="inline-flex items-center text-sm text-gray-400 hover:text-white transition-colors"
        >
          <svg className="w-4 h-4 mr-1" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
          </svg>
          Back to Task
        </Link>
        <div className="p-6 bg-red-500/10 border border-red-500/30 rounded-xl text-red-400">
          <h2 className="text-lg font-semibold mb-2">Error Loading Run</h2>
          <p className="text-sm mb-4">{error || 'Run not found'}</p>
          <button
            onClick={() => loadRunData(true)}
            className="px-4 py-2 bg-red-500/20 hover:bg-red-500/30 text-red-300 rounded-lg text-sm font-medium transition-colors"
          >
            Try Again
          </button>
        </div>
      </div>
    );
  }

  const isActive = ['queued', 'running', 'stopping'].includes(
    run.status.toLowerCase()
  );

  const agentConfig = run.execution_input?.agent_config as Record<string, unknown> | undefined;
  const isOrchestrator = agentConfig?.executor === 'orchestrator' || agentConfig?.executor === 'orchestration' || workItems.length > 0;
  const isAgentRun = isOrchestrator || agentConfig?.executor === 'agent' || modelCalls.length > 0 || toolCalls.length > 0;
  const isMockProvider = agentConfig?.provider === 'mock' || !agentConfig?.provider;
  const totalTokens = modelCalls.reduce((acc, c) => acc + (c.total_tokens || 0), 0);

  return (
    <div className="space-y-8">
      {/* Breadcrumb Navigation */}
      <div className="flex items-center space-x-2 text-sm text-gray-400">
        <Link href="/tasks" className="hover:text-white transition-colors">
          Tasks
        </Link>
        <span>/</span>
        <Link href={`/tasks/${taskId}`} className="hover:text-white transition-colors">
          Task {taskId.slice(0, 8)}
        </Link>
        <span>/</span>
        <span className="text-gray-200 font-mono font-medium">
          Run {run.id.slice(0, 8)}
        </span>
        <Link href={`/workbench/tasks/${taskId}/runs/${runId}`} className="text-blue-300 hover:text-blue-200">
          在统一工作台打开
        </Link>
      </div>

      {/* Provider Info Banner */}
      {isAgentRun && (
        <div>
          {isMockProvider ? (
            <div className="p-4 bg-amber-500/10 border border-amber-500/30 rounded-xl text-amber-300 flex items-center justify-between">
              <div className="flex items-center space-x-3">
                <span className="px-2 py-0.5 text-xs font-semibold uppercase bg-amber-500/20 text-amber-200 rounded border border-amber-500/40">
                  模拟 / Mock Provider
                </span>
                <span className="text-sm">
                  当前为本地内置预设回复，场景：<strong className="font-mono text-white">{String(agentConfig?.mock_scenario || 'normal')}</strong>
                </span>
              </div>
              <span className="text-xs text-amber-400/80 hidden sm:inline">无外部 LLM API 消耗</span>
            </div>
          ) : (
            <div className="p-4 bg-blue-500/10 border border-blue-500/30 rounded-xl text-blue-300 flex items-center justify-between">
              <div className="flex items-center space-x-3">
                <span className="px-2 py-0.5 text-xs font-semibold uppercase bg-blue-500/20 text-blue-200 rounded border border-blue-500/40">
                  真实模型 API
                </span>
                <span className="text-sm">
                  {agentConfig?.api_format === 'responses' ? 'Responses API' : 'Chat Completions API'}
                  {agentConfig?.model ? ` · ${String(agentConfig.model)}` : ''}
                </span>
              </div>
              {totalTokens > 0 && (
                <span className="text-xs font-mono text-blue-200">
                  已报告 Token: {totalTokens.toLocaleString()}{modelCalls.some(c => !c.usage_known) ? "（部分调用未报告用量）" : ""}
                </span>
              )}
            </div>
          )}
        </div>
      )}

      {/* Scope Assets */}
      {runAssets.length > 0 && (
        <div className="p-4 bg-indigo-500/10 border border-indigo-500/30 rounded-xl space-y-2.5">
          <div className="flex items-center justify-between">
            <span className="text-xs font-semibold uppercase tracking-wider text-indigo-300 flex items-center gap-1.5">
              <svg className="w-4 h-4 text-indigo-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
              </svg>
              Assets In Scope ({runAssets.length})
            </span>
            <span className="text-[11px] text-gray-400">Click asset to view full history & observations</span>
          </div>
          <div className="flex flex-wrap gap-2">
            {runAssets.map((a) => (
              <Link
                key={a.id}
                href={`/assets/${a.id}`}
                className="px-3 py-1 rounded-lg bg-gray-900 border border-gray-700/80 hover:border-indigo-500 text-xs font-mono text-gray-200 hover:text-indigo-300 transition-all flex items-center gap-2 group shadow-sm"
              >
                <span className="w-1.5 h-1.5 rounded-full bg-indigo-400 group-hover:scale-125 transition-transform" />
                <span className="font-sans font-medium">{a.name}</span>
                <span className="text-gray-500 font-mono text-[11px]">({a.external_key})</span>
              </Link>
            ))}
          </div>
        </div>
      )}

      {/* Run Top Banner */}
      <div className="bg-gray-900 border border-gray-800 rounded-2xl p-6 shadow-sm space-y-6">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 pb-6 border-b border-gray-800">
          <div className="space-y-2">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-2xl font-bold text-white tracking-tight font-mono">
                Run {run.id}
              </h1>
              <RunStatusBadge status={run.status} finishReason={run.finish_reason} size="lg" />
              <span className="px-2.5 py-1 rounded-lg text-xs font-mono font-bold bg-amber-500/10 text-amber-300 border border-amber-500/30">
                Epoch #{(run as any).current_epoch || 1}
              </span>
            </div>
            {isActive && (
              <p className="text-xs text-blue-400 flex items-center space-x-1.5 animate-pulse">
                <span className="w-2 h-2 rounded-full bg-blue-400" />
                <span>Execution in progress (polling live events every 2s)</span>
              </p>
            )}
          </div>

          {/* Action: Run Controls */}
          {(run.status === 'running' || run.status === 'paused') && (
            <div className="flex items-center gap-2">
              {run.status === 'running' ? (
                <button
                  type="button"
                  onClick={handlePause}
                  disabled={controlPending}
                  className="px-3.5 py-2 bg-amber-600/90 hover:bg-amber-500 text-white text-sm font-medium rounded-lg transition-colors flex items-center space-x-1.5 shadow-sm"
                >
                  <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M10 9v6m4-6v6" />
                  </svg>
                  <span>Pause (暂停)</span>
                </button>
              ) : (
                <button
                  type="button"
                  onClick={handleResume}
                  disabled={controlPending}
                  className="px-3.5 py-2 bg-emerald-600/90 hover:bg-emerald-500 text-white text-sm font-medium rounded-lg transition-colors flex items-center space-x-1.5 shadow-sm"
                >
                  <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M14.752 11.168l-3.197-2.132A1 1 0 0010 9.87v4.263a1 1 0 001.555.832l3.197-2.132a1 1 0 000-1.664z" />
                  </svg>
                  <span>Resume (恢复)</span>
                </button>
              )}
              <button
                type="button"
                onClick={handleStopRun}
                disabled={stopping || controlPending}
                className="px-3.5 py-2 bg-red-600/90 hover:bg-red-500 text-white text-sm font-medium rounded-lg transition-colors flex items-center space-x-1.5 shadow-sm disabled:opacity-50"
              >
                {stopping ? (
                  <>
                    <svg className="animate-spin h-4 w-4 text-white" viewBox="0 0 24 24" fill="none">
                      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                      <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
                    </svg>
                    <span>Stopping...</span>
                  </>
                ) : (
                  <>
                    <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                      <rect x="6" y="6" width="12" height="12" rx="2" fill="currentColor" />
                    </svg>
                    <span>Stop Run</span>
                  </>
                )}
              </button>
            </div>
          )}
        </div>

        {/* Stop Run Error */}
        {stopError && (
          <div className="p-3 bg-red-500/10 border border-red-500/30 text-red-400 rounded-lg text-sm flex items-start space-x-2">
            <svg className="w-5 h-5 flex-shrink-0 text-red-400 mt-0.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
            <span>{stopError}</span>
          </div>
        )}

        {/* Finish Reason Alert / Note */}
        {run.finish_reason && (
          <div className="p-4 bg-gray-950 border border-gray-800 rounded-xl flex items-start space-x-3">
            <div className="p-1 rounded bg-gray-800 text-gray-300 mt-0.5">
              <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
              </svg>
            </div>
            <div>
              <h2 className="text-xs font-semibold uppercase tracking-wider text-gray-400">
                Finish Reason
              </h2>
              <p className="text-sm font-mono text-gray-200 mt-0.5">
                {run.finish_reason}
              </p>
            </div>
          </div>
        )}

        {/* Run Metadata Grid */}
        <div className="grid grid-cols-2 md:grid-cols-4 gap-4">
          <div className="bg-gray-950 p-3.5 rounded-xl border border-gray-800">
            <span className="text-xs text-gray-500 block mb-1">Started At</span>
            <span className="text-xs sm:text-sm font-mono text-gray-200">
              {run.started_at ? new Date(run.started_at).toLocaleTimeString() : 'Pending'}
            </span>
            {run.started_at && (
              <span className="text-[10px] text-gray-500 block mt-0.5">
                {new Date(run.started_at).toLocaleDateString()}
              </span>
            )}
          </div>

          <div className="bg-gray-950 p-3.5 rounded-xl border border-gray-800">
            <span className="text-xs text-gray-500 block mb-1">Finished At</span>
            <span className="text-xs sm:text-sm font-mono text-gray-200">
              {run.finished_at ? new Date(run.finished_at).toLocaleTimeString() : '-'}
            </span>
            {run.finished_at && (
              <span className="text-[10px] text-gray-500 block mt-0.5">
                {new Date(run.finished_at).toLocaleDateString()}
              </span>
            )}
          </div>

          <div className="bg-gray-950 p-3.5 rounded-xl border border-gray-800">
            <span className="text-xs text-gray-500 block mb-1">Duration</span>
            <span className="text-xs sm:text-sm font-mono text-gray-200">
              {calculateDuration()}
            </span>
          </div>

          <div className="bg-gray-950 p-3.5 rounded-xl border border-gray-800">
            <span className="text-xs text-gray-500 block mb-1">Executor Mode</span>
            <span className="text-xs sm:text-sm font-mono text-gray-200">
              {isOrchestrator ? 'Orchestrator (编排)' : isAgentRun ? 'Agent Runner' : 'Fake Worker'}
            </span>
            {totalTokens > 0 && (
              <span className="text-[10px] text-emerald-400 block mt-0.5">
                {totalTokens} tokens
              </span>
            )}
          </div>
        </div>

        {/* Collapsible config_snapshot JSON viewer */}
        <div className="border border-gray-800 rounded-xl overflow-hidden bg-gray-950">
          <button
            type="button"
            onClick={() => setConfigOpen((prev) => !prev)}
            className="w-full px-4 py-3 flex items-center justify-between text-left hover:bg-gray-900/60 transition-colors"
          >
            <div className="flex items-center space-x-2">
              <span className="text-xs font-semibold uppercase tracking-wider text-gray-400">
                Configuration & Execution Input Snapshot
              </span>
            </div>
            <svg
              className={`w-4 h-4 text-gray-400 transform transition-transform duration-200 ${
                configOpen ? 'rotate-180' : ''
              }`}
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
            >
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
            </svg>
          </button>

          {configOpen && (
            <div className="p-4 border-t border-gray-800 bg-gray-950 overflow-x-auto space-y-4">
              <div>
                <h3 className="text-xs text-gray-400 font-semibold mb-1">execution_input</h3>
                <pre className="text-xs font-mono text-gray-300">
                  {JSON.stringify(run.execution_input || {}, null, 2)}
                </pre>
              </div>
              <div className="pt-2 border-t border-gray-800">
                <h3 className="text-xs text-gray-400 font-semibold mb-1">config_snapshot (Legacy)</h3>
                <pre className="text-xs font-mono text-gray-300">
                  {JSON.stringify(run.config_snapshot || {}, null, 2)}
                </pre>
              </div>
            </div>
          )}
        </div>
      </div>

      {/* Final Output / Assistant Answer Section */}
      {run.final_output && (
        <div className="bg-gray-900 border border-emerald-500/30 rounded-2xl p-6 shadow-sm space-y-3">
          <div className="flex items-center space-x-2">
            <span className="w-2.5 h-2.5 rounded-full bg-emerald-400" />
            <h2 className="text-lg font-bold text-white">Assistant Final Answer (最终回答)</h2>
          </div>
          <div className="p-4 bg-gray-950 border border-gray-800 rounded-xl text-gray-200 text-sm whitespace-pre-wrap font-sans leading-relaxed">
            {run.final_output}
          </div>
        </div>
      )}

      <CheckResultsPanel runId={runId} status={run.status} toolCalls={toolCalls} />

      <ExplorationResources runId={runId} onChanged={() => loadRunData()} />

      {/* Tabs for Timeline / Orchestration / Agent Details */}
      <div className="space-y-4">
        <div className="flex border-b border-gray-800 space-x-6 overflow-x-auto pb-px">
          <button
            type="button"
            onClick={() => setActiveTab('goals_graph')}
            className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
              activeTab === 'goals_graph'
                ? 'border-indigo-500 text-indigo-400'
                : 'border-transparent text-gray-400 hover:text-white'
            }`}
          >
            <span className="flex items-center gap-1.5">
              <span className="w-2 h-2 rounded-full bg-indigo-400 animate-pulse"></span>
              <span>目标与探索图 (Goals & Graph)</span>
            </span>
            <span className="text-xs px-2 py-0.5 rounded-full bg-indigo-500/20 text-indigo-300 font-mono">
              {goals.length + graphNodes.length}
            </span>
          </button>

          <button
            type="button"
            onClick={() => setActiveTab('control_budget')}
            className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
              activeTab === 'control_budget'
                ? 'border-amber-500 text-amber-400'
                : 'border-transparent text-gray-400 hover:text-white'
            }`}
          >
            <span>执行控制与代次 (Control & Epochs)</span>
            <span className="text-xs px-2 py-0.5 rounded-full bg-amber-500/20 text-amber-300 font-mono">
              E{epochs.length || 1}
            </span>
          </button>

          {(discoveries.length > 0 || (run?.execution_input as any)?.discovery_policy) && (
            <button
              type="button"
              onClick={() => setActiveTab('discoveries')}
              className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                activeTab === 'discoveries'
                  ? 'border-indigo-500 text-indigo-400'
                  : 'border-transparent text-gray-400 hover:text-white'
              }`}
            >
              <span className="flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full bg-indigo-400 animate-pulse"></span>
                <span>页面链接发现 (Discovery)</span>
              </span>
              <span className="text-xs px-2 py-0.5 rounded-full bg-indigo-500/20 text-indigo-300 font-mono">
                {discoveries.length}
              </span>
            </button>
          )}

          {workItems.length > 0 && (
            <button
              type="button"
              onClick={() => setActiveTab('work_items')}
              className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                activeTab === 'work_items'
                  ? 'border-indigo-500 text-indigo-400'
                  : 'border-transparent text-gray-400 hover:text-white'
              }`}
            >
              <span>Work Items (工作列表)</span>
              <span className="text-xs px-2 py-0.5 rounded-full bg-indigo-500/20 text-indigo-300">
                {workItems.length}
              </span>
            </button>
          )}

          {observations.length > 0 && (
            <button
              type="button"
              onClick={() => setActiveTab('observations')}
              className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                activeTab === 'observations'
                  ? 'border-emerald-500 text-emerald-400'
                  : 'border-transparent text-gray-400 hover:text-white'
              }`}
            >
              <span>Observations (证据与观察)</span>
              <span className="text-xs px-2 py-0.5 rounded-full bg-emerald-500/20 text-emerald-300">
                {observations.length}
              </span>
            </button>
          )}

          {sessions.length > 0 && (
            <button
              type="button"
              onClick={() => setActiveTab('sessions')}
              className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                activeTab === 'sessions'
                  ? 'border-purple-500 text-purple-400'
                  : 'border-transparent text-gray-400 hover:text-white'
              }`}
            >
              <span>Sessions (会话隔离)</span>
              <span className="text-xs px-2 py-0.5 rounded-full bg-purple-500/20 text-purple-300">
                {sessions.length}
              </span>
            </button>
          )}

          <button
            type="button"
            onClick={() => setActiveTab('timeline')}
            className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
              activeTab === 'timeline'
                ? 'border-blue-500 text-blue-400'
                : 'border-transparent text-gray-400 hover:text-white'
            }`}
          >
            <span>Events Timeline</span>
            <span className="text-xs px-2 py-0.5 rounded-full bg-gray-800 text-gray-400">
              {events.length}
            </span>
          </button>

          {isAgentRun && (
            <>
              <button
                type="button"
                onClick={() => setActiveTab('model_calls')}
                className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                  activeTab === 'model_calls'
                    ? 'border-blue-500 text-blue-400'
                    : 'border-transparent text-gray-400 hover:text-white'
                }`}
              >
                <span>Model Calls</span>
                <span className="text-xs px-2 py-0.5 rounded-full bg-gray-800 text-gray-400">
                  {modelCalls.length}
                </span>
              </button>

              <button
                type="button"
                onClick={() => setActiveTab('tool_calls')}
                className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                  activeTab === 'tool_calls'
                    ? 'border-blue-500 text-blue-400'
                    : 'border-transparent text-gray-400 hover:text-white'
                }`}
              >
                <span>Tool Calls</span>
                <span className="text-xs px-2 py-0.5 rounded-full bg-gray-800 text-gray-400">
                  {toolCalls.length}
                </span>
              </button>

              <button
                type="button"
                onClick={() => setActiveTab('messages')}
                className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 whitespace-nowrap ${
                  activeTab === 'messages'
                    ? 'border-blue-500 text-blue-400'
                    : 'border-transparent text-gray-400 hover:text-white'
                }`}
              >
                <span>Conversation</span>
                <span className="text-xs px-2 py-0.5 rounded-full bg-gray-800 text-gray-400">
                  {messages.length}
                </span>
              </button>
            </>
          )}
        </div>

        {/* Tab Content */}
        {activeTab === 'goals_graph' && (
          <div className="space-y-6">
            {/* Section 1: Goals */}
            <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
              <div className="flex items-center justify-between">
                <h3 className="text-sm font-bold text-white flex items-center gap-2">
                  <span className="w-2 h-2 rounded-full bg-indigo-400"></span>
                  <span>目标与证明驱动 (Goal-Driven Exploration)</span>
                </h3>
                <span className="text-xs text-gray-400 font-mono">
                  已证明目标: {goals.filter(g => g.state === 'met').length} / {goals.length}
                </span>
              </div>

              {goals.length === 0 ? (
                <div className="p-6 text-center text-gray-500 border border-gray-800/80 rounded-lg text-xs">
                  当前 Run 尚未定义独立目标。
                </div>
              ) : (
                <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                  {goals.map((g) => (
                    <div key={g.id} className="p-4 bg-gray-950 border border-gray-800 rounded-lg space-y-2">
                      <div className="flex items-center justify-between">
                        <span className="font-semibold text-white text-sm">{String(g.payload.text || '未命名目标')}</span>
                        <div className="flex items-center gap-2">
                          <span className={`text-xs font-semibold px-2 py-0.5 rounded border ${
                            g.state === 'met'
                              ? 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40'
                              : 'bg-amber-500/20 text-amber-300 border-amber-500/40'
                          }`}>
                            {g.state === 'met' ? '已证明 (Proved)' : '未达成 (Open)'}
                          </span>

                        </div>
                      </div>
                      <p className="text-xs text-gray-400 leading-relaxed">{String(g.payload.vulnclass || '')}</p>
                      {g.completed_at && (
                        <p className="text-[10px] font-mono text-emerald-400">
                          证明于: {new Date(g.completed_at).toLocaleString()}
                        </p>
                      )}
                    </div>
                  ))}
                </div>
              )}
            </div>

            {/* Section 2: Exploration Graph Nodes */}
            <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
              <div className="flex items-center justify-between">
                <h3 className="text-sm font-bold text-white flex items-center gap-2">
                  <span className="w-2 h-2 rounded-full bg-blue-400"></span>
                  <span>探索图节点与事实线索 (Exploration Graph Nodes)</span>
                </h3>
                <span className="text-xs text-gray-400 font-mono">
                  当前页节点: {graphNodes.length}（完整分页见上方探索关系）
                </span>
              </div>

              {graphNodes.length === 0 ? (
                <div className="p-6 text-center text-gray-500 border border-gray-800/80 rounded-lg text-xs">
                  探索图尚未生成节点。
                </div>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-left text-xs font-mono">
                    <thead>
                      <tr className="border-b border-gray-800 text-gray-400">
                        <th className="py-2.5 px-3">类型 (Kind)</th>
                        <th className="py-2.5 px-3">状态</th>
                        <th className="py-2.5 px-3">代次</th>
                        <th className="py-2.5 px-3">来源 / 属性</th>
                        <th className="py-2.5 px-3">内容摘要 (Payload)</th>
                        <th className="py-2.5 px-3">时间</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-gray-800/60">
                      {graphNodes.map((n) => {
                        const kindColor: Record<string, string> = {
                          begin: 'bg-gray-800 text-gray-300 border-gray-700',
                          goal: 'bg-indigo-500/20 text-indigo-300 border-indigo-500/40',
                          intent: 'bg-blue-500/20 text-blue-300 border-blue-500/40',
                          fact: 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40',
                          finding: 'bg-purple-500/20 text-purple-300 border-purple-500/40',
                          hint: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
                          digest: 'bg-cyan-500/20 text-cyan-300 border-cyan-500/40',
                        };
                        return (
                          <tr key={n.id} className="hover:bg-gray-950/60 transition-colors">
                            <td className="py-2.5 px-3">
                              <span className={`px-2 py-0.5 rounded text-[11px] font-bold border ${kindColor[n.kind] || 'bg-gray-800 text-gray-300'}`}>
                                {n.kind.toUpperCase()}
                              </span>
                            </td>
                            <td className="py-2.5 px-3 text-gray-300">{n.state}</td>
                            <td className="py-2.5 px-3 text-amber-300 font-bold">E{n.epoch_number || 1}</td>
                            <td className="py-2.5 px-3 text-gray-400">{n.origin || '-'}</td>
                            <td className="py-2.5 px-3 text-gray-300 font-sans max-w-md truncate">
                              {n.payload?.title || n.payload?.text || n.payload?.fact || n.payload?.description || JSON.stringify(n.payload)}
                            </td>
                            <td className="py-2.5 px-3 text-gray-500">{new Date(n.created_at).toLocaleTimeString()}</td>
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          </div>
        )}

        {activeTab === 'control_budget' && (
          <div className="space-y-6">
            {/* Section 1: Effective Budget & Grants */}
            <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
              <div className="flex items-center justify-between">
                <h3 className="text-sm font-bold text-white flex items-center gap-2">
                  <span className="w-2 h-2 rounded-full bg-amber-400"></span>
                  <span>有效预算与人工授权账本 (Authorized Budget Ledger)</span>
                </h3>
                <button
                  type="button"
                  onClick={() => setGrantFormOpen(!grantFormOpen)}
                  className="px-3 py-1.5 bg-amber-600 hover:bg-amber-500 text-white rounded text-xs font-medium transition-colors shadow-sm"
                >
                  {grantFormOpen ? '收起追加表单' : '追加预算授权 (Grant Budget)'}
                </button>
              </div>

              {effectiveBudget && (
                <div className="grid grid-cols-3 gap-4 bg-gray-950 p-4 rounded-lg border border-gray-800 text-center">
                  <div>
                    <span className="text-xs text-gray-500 block">有效模型调用上限</span>
                    <span className="text-xl font-mono font-bold text-white mt-1 block">{effectiveBudget.max_model_calls}</span>
                  </div>
                  <div>
                    <span className="text-xs text-gray-500 block">有效工具调用上限</span>
                    <span className="text-xl font-mono font-bold text-white mt-1 block">{effectiveBudget.max_tool_calls}</span>
                  </div>
                  <div>
                    <span className="text-xs text-gray-500 block">超时上限 (秒)</span>
                    <span className="text-xl font-mono font-bold text-white mt-1 block">{effectiveBudget.run_timeout_seconds}s</span>
                  </div>
                </div>
              )}

              {grantFormOpen && (
                <div className="p-4 bg-gray-950 border border-amber-500/40 rounded-lg space-y-3">
                  <h4 className="text-xs font-bold text-amber-300">人工授权追加执行额度 (无需新建子 Run)</h4>
                  <div className="grid grid-cols-1 sm:grid-cols-3 gap-3">
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">追加模型调用次数</label>
                      <input
                        type="number"
                        min="0"
                        value={grantModelCalls}
                        onChange={e => setGrantModelCalls(parseInt(e.target.value) || 0)}
                        className="w-full text-xs bg-gray-900 border border-gray-700 rounded px-2.5 py-1.5 text-white"
                      />
                    </div>
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">追加工具调用次数</label>
                      <input
                        type="number"
                        min="0"
                        value={grantToolCalls}
                        onChange={e => setGrantToolCalls(parseInt(e.target.value) || 0)}
                        className="w-full text-xs bg-gray-900 border border-gray-700 rounded px-2.5 py-1.5 text-white"
                      />
                    </div>
                    <div>
                      <label className="text-[11px] text-gray-400 block mb-1">追加运行秒数</label>
                      <input
                        type="number"
                        min="0"
                        value={grantSeconds}
                        onChange={e => setGrantSeconds(parseInt(e.target.value) || 0)}
                        className="w-full text-xs bg-gray-900 border border-gray-700 rounded px-2.5 py-1.5 text-white"
                      />
                    </div>
                  </div>
                  <div>
                    <label className="text-[11px] text-gray-400 block mb-1">授权理由说明</label>
                    <input
                      type="text"
                      value={grantReason}
                      onChange={e => setGrantReason(e.target.value)}
                      className="w-full text-xs bg-gray-900 border border-gray-700 rounded px-2.5 py-1.5 text-white"
                    />
                  </div>
                  <div className="flex justify-end pt-1">
                    <button
                      type="button"
                      onClick={handleGrantBudget}
                      disabled={controlPending || !grantReason.trim()}
                      className="px-4 py-1.5 bg-amber-600 hover:bg-amber-500 text-white rounded text-xs font-medium transition-colors shadow"
                    >
                      提交追加授权
                    </button>
                  </div>
                </div>
              )}

              {/* Grants Table */}
              <div className="overflow-x-auto">
                <table className="w-full text-left text-xs font-mono">
                  <thead>
                    <tr className="border-b border-gray-800 text-gray-400">
                      <th className="py-2 px-3">代次</th>
                      <th className="py-2 px-3">追加模型</th>
                      <th className="py-2 px-3">追加工具</th>
                      <th className="py-2 px-3">追加秒数</th>
                      <th className="py-2 px-3">授权理由</th>
                      <th className="py-2 px-3">授权人</th>
                      <th className="py-2 px-3">时间</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-gray-800/60">
                    {budgetGrants.length === 0 ? (
                      <tr>
                        <td colSpan={7} className="py-4 text-center text-gray-500 italic">尚无追加授权记录。</td>
                      </tr>
                    ) : (
                      budgetGrants.map(g => (
                        <tr key={g.id}>
                          <td className="py-2 px-3 text-amber-300">E{g.epoch_number}</td>
                          <td className="py-2 px-3 text-emerald-400">+{g.additional_model_calls}</td>
                          <td className="py-2 px-3 text-emerald-400">+{g.additional_tool_calls}</td>
                          <td className="py-2 px-3 text-emerald-400">+{g.additional_seconds}s</td>
                          <td className="py-2 px-3 text-gray-300 font-sans">{g.reason}</td>
                          <td className="py-2 px-3 text-gray-400">{g.granted_by}</td>
                          <td className="py-2 px-3 text-gray-500">{new Date(g.created_at).toLocaleTimeString()}</td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            </div>

            {/* Section 2: Execution Epochs */}
            <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
              <div className="flex items-center justify-between">
                <div>
                  <h3 className="text-sm font-bold text-white flex items-center gap-2">
                    <span className="w-2 h-2 rounded-full bg-purple-400"></span>
                    <span>执行代次隔离 (Execution Epochs)</span>
                  </h3>
                  <p className="text-[11px] text-gray-400 mt-0.5">当前代次: Epoch #{(run as any).current_epoch || 1}。旧代次迟到的写入将被强制阻断以防污染。</p>
                </div>
                <button
                  type="button"
                  onClick={handleStartEpoch}
                  disabled={controlPending || !newEpochReason.trim() || ['completed', 'stopping'].includes(run.status)}
                  className="px-3 py-1.5 bg-purple-700 hover:bg-purple-600 text-white rounded text-xs font-medium transition-colors shadow-sm"
                >
                  推进新执行代次 (Start New Epoch)
                </button>
              </div>

              <div className="overflow-x-auto">
                <table className="w-full text-left text-xs font-mono">
                  <thead>
                    <tr className="border-b border-gray-800 text-gray-400">
                      <th className="py-2 px-3">代次编号</th>
                      <th className="py-2 px-3">状态</th>
                      <th className="py-2 px-3">启动时间</th>
                      <th className="py-2 px-3">结束时间</th>
                      <th className="py-2 px-3">结束原因</th>
                      <th className="py-2 px-3">总结说明</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-gray-800/60">
                    {epochs.length === 0 ? (
                      <tr>
                        <td colSpan={6} className="py-4 text-center text-gray-500 italic">尚无代次变更记录。</td>
                      </tr>
                    ) : (
                      epochs.map(ep => (
                        <tr key={ep.id}>
                          <td className="py-2 px-3 text-purple-300 font-bold">Epoch #{ep.epoch_number}</td>
                          <td className="py-2 px-3">
                            <span className={`px-2 py-0.5 rounded text-[11px] font-bold ${ep.status === 'running' ? 'bg-emerald-500/20 text-emerald-300' : 'bg-gray-800 text-gray-400'}`}>
                              {ep.status}
                            </span>
                          </td>
                          <td className="py-2 px-3 text-gray-400">{new Date(ep.started_at).toLocaleTimeString()}</td>
                          <td className="py-2 px-3 text-gray-400">{ep.finished_at ? new Date(ep.finished_at).toLocaleTimeString() : '-'}</td>
                          <td className="py-2 px-3 text-gray-300 font-sans">{ep.finish_reason || '-'}</td>
                          <td className="py-2 px-3 text-gray-300 font-sans">{ep.summary || '-'}</td>
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        )}

        {/* Tab Content */}
        {activeTab === 'discoveries' && (
          <div className="space-y-6">
            {/* Policy & Seeds Banner */}
            {(() => {
              const execInput = run?.execution_input as any;
              const policy = execInput?.discovery_policy;
              const seeds = execInput?.scope?.seeds as string[] | undefined;

              return (
                <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
                  <div className="flex items-center justify-between border-b border-gray-800 pb-3">
                    <div className="flex items-center gap-2">
                      <span className="w-2.5 h-2.5 rounded-full bg-indigo-400"></span>
                      <h3 className="text-sm font-bold text-white uppercase tracking-wider">
                        自动发现策略与种子边界 (Discovery Bounds)
                      </h3>
                    </div>
                    <span className="text-xs font-mono text-gray-400 bg-gray-950 px-2.5 py-1 rounded border border-gray-800">
                      Mode: discovery
                    </span>
                  </div>

                  <div className="grid grid-cols-1 md:grid-cols-2 gap-4 text-xs">
                    <div className="p-3 bg-gray-950/70 border border-gray-800/80 rounded-lg space-y-1.5">
                      <span className="font-semibold text-emerald-400 block">🌱 初始种子资产 (Seeds):</span>
                      <div className="flex flex-wrap gap-1.5 pt-0.5">
                        {(seeds || ['/']).map((s) => (
                          <span key={s} className="font-mono bg-emerald-500/10 text-emerald-300 px-2 py-0.5 rounded border border-emerald-500/30">
                            {s} (首页)
                          </span>
                        ))}
                      </div>
                      <p className="text-[11px] text-gray-500 pt-1">
                        Run 创建初始仅关联此种子资产；全局历史资产严格隔离，不预先填入。
                      </p>
                    </div>

                    <div className="p-3 bg-gray-950/70 border border-gray-800/80 rounded-lg space-y-1.5 font-mono">
                      <span className="font-semibold text-indigo-400 block font-sans">🛡️ 允许发现策略 (Discovery Policy):</span>
                      <div className="text-gray-300 space-y-0.5 text-[11px]">
                        <div>Origin: <span className="text-gray-400">{policy?.origin || 'http://127.0.0.1:29090'}</span></div>
                        <div>Method: <span className="text-gray-400">{policy?.allowed_methods?.join(', ') || 'GET'}</span></div>
                        <div>Allowed Paths: <span className="text-indigo-300 font-semibold">{policy?.allowed_paths?.join(', ') || '/, /login, /health'}</span></div>
                      </div>
                      <p className="text-[11px] text-gray-500 font-sans pt-1">
                        超出上述精确路径范围或非允许协议/域名的候选链接，一律在发包前拦截拒绝。
                      </p>
                    </div>
                  </div>
                </div>
              );
            })()}

            {/* Hierarchical Tree of Discovered Endpoints */}
            <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
              <h3 className="text-xs font-bold text-gray-400 uppercase tracking-wider flex items-center gap-2">
                <svg className="w-4 h-4 text-indigo-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 6h16M4 12h16M4 18h7" />
                </svg>
                <span>发现链路拓扑 (Discovery Provenance Tree)</span>
              </h3>

              <div className="p-4 bg-gray-950 rounded-xl border border-gray-800/80 space-y-3 font-mono text-xs">
                {/* Root Home Seed */}
                <div className="flex items-center gap-2 text-white font-semibold">
                  <span className="px-2 py-0.5 rounded bg-emerald-500/20 text-emerald-400 border border-emerald-500/30 text-[11px]">
                    SEED 入口
                  </span>
                  <span>首页 / (Local Fixture Home)</span>
                </div>

                {/* Level 1 Discoveries (e.g. /login) */}
                {discoveries
                  .filter((d) => (d.source_asset_name && d.source_asset_name.includes('Home')) || (d.source_asset_key && d.source_asset_key.endsWith('/')))
                  .map((d1) => (
                    <div key={d1.id} className="ml-6 pl-4 border-l-2 border-indigo-500/40 space-y-3">
                      <div className="flex items-center gap-2 text-indigo-300">
                        <span className="text-indigo-400 font-bold">↳</span>
                        <span className="px-2 py-0.5 rounded bg-indigo-500/20 text-indigo-300 border border-indigo-500/30 text-[11px]">
                          发现端点
                        </span>
                        <Link href={`/assets/${d1.target_asset_id}`} className="underline hover:text-white font-semibold">
                          {d1.target_asset_name || d1.target_asset_key}
                        </Link>
                        <span className="text-gray-500 text-[11px]">
                          (via <code className="text-gray-300 bg-gray-900 px-1 rounded">{d1.raw_href}</code>)
                        </span>
                      </div>

                      {/* Level 2 Discoveries (e.g. /health from /login) */}
                      {discoveries
                        .filter((d2) => d2.source_asset_id === d1.target_asset_id)
                        .map((d2) => (
                          <div key={d2.id} className="ml-6 pl-4 border-l-2 border-purple-500/40">
                            <div className="flex items-center gap-2 text-purple-300">
                              <span className="text-purple-400 font-bold">↳</span>
                              <span className="px-2 py-0.5 rounded bg-purple-500/20 text-purple-300 border border-purple-500/30 text-[11px]">
                                深入发现
                              </span>
                              <Link href={`/assets/${d2.target_asset_id}`} className="underline hover:text-white font-semibold">
                                {d2.target_asset_name || d2.target_asset_key}
                              </Link>
                              <span className="text-gray-500 text-[11px]">
                                (via <code className="text-gray-300 bg-gray-900 px-1 rounded">{d2.raw_href}</code>)
                              </span>
                            </div>
                          </div>
                        ))}
                    </div>
                  ))}

                {discoveries.length === 0 && (
                  <p className="text-gray-500 italic text-[11px] pt-1">
                    尚未提取到任何链接或页面中不包含更多未处理的合法链接。
                  </p>
                )}
              </div>
            </div>

            {/* Discoveries Provenance Table */}
            <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
              <h3 className="text-xs font-bold text-gray-400 uppercase tracking-wider flex items-center gap-2">
                <svg className="w-4 h-4 text-emerald-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                  <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M13.828 10.172a4 4 0 00-5.656 0l-4 4a4 4 0 105.656 5.656l1.102-1.101m-.758-4.899a4 4 0 005.656 0l4-4a4 4 0 00-5.656-5.656l-1.1 1.1" />
                </svg>
                <span>已登记发现明细 (Asset Discovery Records) ({discoveries.length})</span>
              </h3>

              {discoveries.length === 0 ? (
                <div className="p-6 text-center text-gray-500 border border-gray-800/80 rounded-lg text-xs">
                  暂无新资产发现记录。
                </div>
              ) : (
                <div className="overflow-x-auto">
                  <table className="w-full text-left text-xs border-collapse">
                    <thead>
                      <tr className="border-b border-gray-800 text-gray-400 font-semibold uppercase tracking-wider">
                        <th className="py-2.5 px-3">发现目标资产</th>
                        <th className="py-2.5 px-3">来源前置页面</th>
                        <th className="py-2.5 px-3 font-mono">原始 Href</th>
                        <th className="py-2.5 px-3 font-mono">来源 ToolCall</th>
                        <th className="py-2.5 px-3">发现时间</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-gray-800/60 font-mono">
                      {discoveries.map((d) => (
                        <tr key={d.id} className="hover:bg-gray-850/50 transition-colors">
                          <td className="py-2.5 px-3">
                            <Link href={`/assets/${d.target_asset_id}`} className="text-indigo-400 hover:text-indigo-200 underline font-semibold">
                              {d.target_asset_name || d.target_asset_key}
                            </Link>
                          </td>
                          <td className="py-2.5 px-3">
                            <Link href={`/assets/${d.source_asset_id}`} className="text-gray-300 hover:text-white underline">
                              {d.source_asset_name || d.source_asset_key}
                            </Link>
                          </td>
                          <td className="py-2.5 px-3">
                            <span className="bg-gray-950 px-2 py-0.5 rounded text-amber-300 border border-gray-800">
                              {d.raw_href}
                            </span>
                          </td>
                          <td className="py-2.5 px-3 text-gray-400 text-[11px]">
                            {d.source_tool_call_id.slice(0, 8)}...
                          </td>
                          <td className="py-2.5 px-3 text-gray-400 font-sans text-[11px]">
                            {new Date(d.discovered_at).toLocaleTimeString()}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </div>

            {/* Rejections & Filtered Candidates Section */}
            {(() => {
              const extractCalls = toolCalls.filter((tc) => tc.tool_name === 'extract_endpoint_links');
              const allRejections: Array<{ callId: string; rawHref: string; reason: string }> = [];

              extractCalls.forEach((tc) => {
                try {
                  const out = JSON.parse(tc.output);
                  if (out?.rejected && Array.isArray(out.rejected)) {
                    out.rejected.forEach((r: any) => {
                      allRejections.push({
                        callId: tc.id,
                        rawHref: r.raw_href,
                        reason: r.reason,
                      });
                    });
                  }
                } catch {}
              });

              return (
                <div className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-4">
                  <div className="flex items-center justify-between">
                    <h3 className="text-xs font-bold text-gray-400 uppercase tracking-wider flex items-center gap-2">
                      <svg className="w-4 h-4 text-red-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M18.364 18.364A9 9 0 005.636 5.636m12.728 12.728A9 9 0 015.636 5.636m12.728 12.728L5.636 5.636" />
                      </svg>
                      <span>候选链接过滤与拒绝明细 (Candidate Filter & Rejection Log) ({allRejections.length})</span>
                    </h3>
                    <span className="text-[11px] text-gray-500 font-sans">
                      发包前本地校验与阻断，零越界流量
                    </span>
                  </div>

                  {allRejections.length === 0 ? (
                    <div className="p-6 text-center text-gray-500 border border-gray-800/80 rounded-lg text-xs">
                      无被拒绝的候选链接。
                    </div>
                  ) : (
                    <div className="space-y-2">
                      {allRejections.map((rej, idx) => (
                        <div
                          key={idx}
                          className="p-3 bg-gray-950/80 border border-red-500/20 rounded-lg flex flex-col sm:flex-row sm:items-center justify-between gap-2 text-xs"
                        >
                          <div className="flex items-center gap-2 min-w-0">
                            <span className="px-2 py-0.5 rounded bg-red-500/10 text-red-400 border border-red-500/20 text-[10px] font-bold font-mono">
                              REJECTED
                            </span>
                            <code className="text-amber-300 font-mono text-xs truncate max-w-xs sm:max-w-md">
                              {rej.rawHref}
                            </code>
                          </div>
                          <div className="text-gray-400 text-[11px] italic text-right">
                            {rej.reason}
                          </div>
                        </div>
                      ))}
                    </div>
                  )}
                </div>
              );
            })()}
          </div>
        )}

        {/* Tab Content */}
        {activeTab === 'work_items' && (
          <div className="space-y-4">
            {(workItemDetails.length > 0 ? workItemDetails : workItems).length === 0 ? (
              <div className="p-8 text-center text-gray-500 border border-gray-800 rounded-xl">
                No work items created yet.
              </div>
            ) : (
              (workItemDetails.length > 0 ? workItemDetails : workItems).map((item) => {
                const detail = workItemDetails.find((d) => d.id === item.id) || (item as any);
                return (
                  <div key={item.id} className="p-5 bg-gray-900 border border-gray-800 rounded-xl space-y-3">
                    <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
                      <div className="flex flex-wrap items-center gap-2.5">
                        <span className="font-mono font-semibold text-white text-base">
                          {item.title}
                        </span>
                        <span className="font-mono text-xs text-gray-400 bg-gray-950 px-2 py-0.5 rounded border border-gray-800">
                          {item.key}
                        </span>
                        <span className="text-xs px-2 py-0.5 rounded bg-blue-500/10 text-blue-400 border border-blue-500/20">
                          {item.work_type}
                        </span>
                        {detail.created_round_num !== undefined && (
                          <span className="text-xs px-2 py-0.5 rounded bg-purple-500/10 text-purple-400 border border-purple-500/20">
                            Round {detail.created_round_num}
                          </span>
                        )}
                      </div>
                      <span className={`text-xs font-semibold px-2.5 py-1 rounded inline-flex items-center gap-1.5 ${
                        item.status === 'completed' ? 'bg-green-500/10 text-green-400 border border-green-500/30' :
                        item.status === 'running' ? 'bg-blue-500/10 text-blue-400 border border-blue-500/30 animate-pulse' :
                        item.status === 'blocked' ? 'bg-amber-500/10 text-amber-400 border border-amber-500/30' :
                        item.status === 'exhausted' ? 'bg-orange-500/10 text-orange-300 border border-orange-500/30' :
                        item.status === 'failed' ? 'bg-red-500/10 text-red-400 border border-red-500/30' :
                        'bg-gray-800 text-gray-400'
                      }`}>
                        <span className="w-1.5 h-1.5 rounded-full bg-current" />
                        {item.status.toUpperCase()}
                      </span>
                    </div>

                    <p className="text-sm text-gray-300 font-sans leading-relaxed">
                      {item.objective}
                    </p>

                    {/* Traceability: Target Assets */}
                    {detail.target_assets && detail.target_assets.length > 0 && (
                      <div className="flex flex-wrap items-center gap-2 text-xs text-gray-400">
                        <span className="font-semibold text-gray-400">Target Assets:</span>
                        <div className="flex flex-wrap gap-1.5">
                          {detail.target_assets.map((ta: any) => (
                            <Link
                              key={ta.id}
                              href={`/assets/${ta.id}`}
                              className="font-mono text-[11px] px-2 py-0.5 rounded bg-indigo-500/10 text-indigo-300 hover:text-white border border-indigo-500/20 hover:border-indigo-400 transition-colors"
                            >
                              {ta.name} ({ta.external_key})
                            </Link>
                          ))}
                        </div>
                      </div>
                    )}

                    {/* Traceability: Source Observations */}
                    {detail.source_observations && detail.source_observations.length > 0 && (
                      <div className="flex flex-wrap items-center gap-2 text-xs text-amber-400/90">
                        <span className="font-semibold">Causal Source Obs:</span>
                        <div className="flex flex-wrap gap-1.5">
                          {detail.source_observations.map((so: any) => (
                            <span
                              key={so.id}
                              className="font-mono text-[11px] px-2 py-0.5 rounded bg-amber-500/10 text-amber-300 border border-amber-500/30"
                            >
                              [{so.obs_type}] {so.content.slice(0, 30)}...
                            </span>
                          ))}
                        </div>
                      </div>
                    )}

                    {/* Dependencies */}
                    {detail.dependencies && detail.dependencies.length > 0 && (
                      <div className="flex flex-wrap items-center gap-2 text-xs text-gray-400">
                        <span className="font-semibold text-gray-400">Prerequisites:</span>
                        <div className="flex flex-wrap gap-1.5">
                          {detail.dependencies.map((dep: any) => (
                            <span
                              key={dep.id}
                              className="font-mono text-[11px] px-2 py-0.5 rounded bg-gray-950 text-gray-300 border border-gray-800"
                            >
                              {dep.key}
                            </span>
                          ))}
                        </div>
                      </div>
                    )}

                    {item.blocked_reason && (
                      <div className="p-3 bg-amber-500/10 border border-amber-500/30 rounded-lg text-amber-300 text-xs">
                        <strong>Blocked Reason:</strong> {item.blocked_reason}
                      </div>
                    )}

                    <div className="flex flex-wrap items-center gap-4 text-xs text-gray-400 pt-1 border-t border-gray-800/80">
                      {item.current_attempt_id && (
                        <span className="font-mono">
                          Attempt: <span className="text-gray-200">{item.current_attempt_id.slice(0, 8)}</span>
                        </span>
                      )}
                      <span>Created: {new Date(item.created_at).toLocaleTimeString()}</span>
                    </div>

                    {/* Steer & Kill Controls */}
                    {['pending', 'running', 'blocked', 'exhausted'].includes(item.status) && (
                      <div className="pt-2 flex flex-wrap items-center gap-2">
                        <button
                          onClick={() => setSteerTarget({ id: item.id, title: item.title, instruction: '' })}
                          className="px-2.5 py-1 text-xs rounded bg-amber-500/10 hover:bg-amber-500/20 text-amber-300 border border-amber-500/30 transition-colors"
                        >
                          人工纠偏 (Steer)
                        </button>
                        {['pending', 'running'].includes(item.status) && (
                          <button
                            onClick={() => handleKill(item.id)}
                            className="px-2.5 py-1 text-xs rounded bg-red-500/10 hover:bg-red-500/20 text-red-400 border border-red-500/30 transition-colors"
                          >
                            强制终止 (Kill)
                          </button>
                        )}
                      </div>
                    )}

                    {steerTarget && steerTarget.id === item.id && (
                      <div className="p-3 bg-gray-950 border border-amber-500/40 rounded-lg space-y-2 mt-2">
                        <div className="text-xs font-semibold text-amber-300">
                          向工作项注入人工指导建议 (Steer Instruction)
                        </div>
                        <textarea
                          value={steerTarget.instruction}
                          onChange={(e) => setSteerTarget({ ...steerTarget, instruction: e.target.value })}
                          placeholder="输入指导意见，例如：优先测试 /admin 路径下的导出接口..."
                          rows={2}
                          className="w-full bg-gray-900 border border-gray-700 rounded p-2 text-xs text-white focus:outline-none focus:border-amber-500 font-mono"
                        />
                        <div className="flex justify-end gap-2">
                          <button
                            onClick={() => setSteerTarget(null)}
                            className="px-2.5 py-1 text-xs rounded bg-gray-800 text-gray-300 hover:bg-gray-700"
                          >
                            取消
                          </button>
                          <button
                            onClick={handleSteer}
                            disabled={controlPending || !steerTarget.instruction.trim()}
                            className="px-2.5 py-1 text-xs rounded bg-amber-600 text-white hover:bg-amber-500 disabled:opacity-50"
                          >
                            提交纠偏
                          </button>
                        </div>
                      </div>
                    )}

                    {item.output_data && Object.keys(item.output_data).length > 0 && (
                      <div className="pt-2">
                        <span className="text-[11px] text-gray-400 font-semibold block mb-1">Output Data:</span>
                        <pre className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-emerald-300 overflow-x-auto max-h-48">
                          {JSON.stringify(item.output_data, null, 2)}
                        </pre>
                      </div>
                    )}
                  </div>
                );
              })
            )}
          </div>
        )}

        {activeTab === 'observations' && (
          <div className="space-y-4">
            {observations.length === 0 ? (
              <div className="p-8 text-center text-gray-500 border border-gray-800 rounded-xl">
                No observations recorded yet.
              </div>
            ) : (
              observations.map((obs) => (
                <div key={obs.id} className="p-4 bg-gray-900 border border-emerald-500/20 rounded-xl space-y-3">
                  <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2">
                    <div className="flex flex-wrap items-center gap-2.5">
                      <span className="px-2 py-0.5 text-xs font-semibold rounded bg-emerald-500/20 text-emerald-300 border border-emerald-500/30 font-mono">
                        {obs.obs_type}
                      </span>
                      <span className="font-mono text-xs text-gray-500">
                        {obs.id.slice(0, 8)}
                      </span>
                      {obs.asset_id && (
                        <Link
                          href={`/assets/${obs.asset_id}`}
                          className="px-2 py-0.5 text-xs font-mono rounded bg-indigo-500/10 text-indigo-300 hover:text-white border border-indigo-500/20 transition-colors flex items-center gap-1"
                        >
                          <span>Asset:</span>
                          <span className="underline">
                            {runAssets.find((a) => a.id === obs.asset_id)?.name || obs.asset_id.slice(0, 8)}
                          </span>
                        </Link>
                      )}
                    </div>
                    <div className="flex items-center space-x-2 text-xs">
                      <span className="text-gray-400">Source Evidence:</span>
                      <span className="px-2 py-0.5 bg-gray-950 text-indigo-300 font-mono text-[11px] rounded border border-indigo-500/30">
                        tool_call #{obs.source_tool_call_id.slice(0, 8)}
                      </span>
                    </div>
                  </div>

                  <p className="text-sm text-gray-200 font-sans">
                    {obs.content}
                  </p>

                  {obs.data && Object.keys(obs.data).length > 0 && (
                    <div className="space-y-2 pt-1">
                      {(Boolean(obs.data.model_extracted) || Boolean(obs.data.http_evidence)) ? (
                        <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                          {Boolean(obs.data.model_extracted) && (
                            <div className="p-3 bg-gray-950 border border-indigo-500/30 rounded-lg space-y-1.5">
                              <span className="text-[11px] font-semibold text-indigo-400 uppercase tracking-wider block">
                                🤖 模型提炼内容 (Model Extracted)
                              </span>
                              <pre className="text-xs font-mono text-indigo-200 overflow-x-auto whitespace-pre-wrap">
                                {JSON.stringify(obs.data.model_extracted, null, 2)}
                              </pre>
                            </div>
                          )}
                          {Boolean(obs.data.http_evidence) && (
                            <div className="p-3 bg-gray-950 border border-emerald-500/30 rounded-lg space-y-1.5">
                              <span className="text-[11px] font-semibold text-emerald-400 uppercase tracking-wider block">
                                🌐 底层真实 HTTP 证据 (Raw Evidence)
                              </span>
                              <pre className="text-xs font-mono text-emerald-200 overflow-x-auto whitespace-pre-wrap">
                                {JSON.stringify(obs.data.http_evidence, null, 2)}
                              </pre>
                            </div>
                          )}
                        </div>
                      ) : (
                        <pre className="p-2 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-gray-300 overflow-x-auto">
                          {JSON.stringify(obs.data, null, 2)}
                        </pre>
                      )}
                    </div>
                  )}

                  <div className="text-[11px] text-gray-500 pt-1">
                    Recorded: {new Date(obs.created_at).toLocaleString()}
                  </div>
                </div>
              ))
            )}
          </div>
        )}

        {activeTab === 'sessions' && (
          <div className="space-y-4">
            {sessions.length === 0 ? (
              <div className="p-8 text-center text-gray-500 border border-gray-800 rounded-xl">
                No sessions recorded.
              </div>
            ) : (
              sessions.map((sess) => (
                <div key={sess.id} className="p-4 bg-gray-900 border border-gray-800 rounded-xl space-y-2">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center space-x-2.5">
                      <span className={`px-2 py-0.5 text-xs font-semibold uppercase rounded font-mono ${
                        sess.role === 'planner'
                          ? 'bg-purple-500/20 text-purple-300 border border-purple-500/30'
                          : 'bg-emerald-500/20 text-emerald-300 border border-emerald-500/30'
                      }`}>
                        {sess.role}
                      </span>
                      <span className="font-mono text-xs text-gray-300">
                        Session {sess.id.slice(0, 8)}
                      </span>
                      {sess.round !== undefined && sess.round !== null && (
                        <span className="text-xs text-gray-400 bg-gray-950 px-2 py-0.5 rounded border border-gray-800">
                          Round {sess.round}
                        </span>
                      )}
                    </div>
                    <span className={`text-xs px-2 py-0.5 rounded ${
                      sess.status === 'completed' ? 'bg-green-500/10 text-green-400' :
                      sess.status === 'running' ? 'bg-blue-500/10 text-blue-400 animate-pulse' :
                      sess.status === 'exhausted' ? 'bg-orange-500/10 text-orange-300' :
                      sess.status === 'failed' ? 'bg-red-500/10 text-red-400' : 'bg-gray-800 text-gray-400'
                    }`}>
                      {sess.status}
                    </span>
                  </div>

                  {sess.finish_reason && (
                    <div className="text-xs text-gray-400 font-mono">
                      finish: <span className="text-gray-200">{sess.finish_reason}</span>
                    </div>
                  )}

                  <div className="text-[11px] text-gray-500 pt-1">
                    Created: {new Date(sess.created_at).toLocaleTimeString()}
                  </div>
                </div>
              ))
            )}
          </div>
        )}

        {activeTab === 'timeline' && (
          <EventTimeline events={events} />
        )}

        {activeTab === 'model_calls' && (
          <div className="space-y-4">
            {modelCalls.length === 0 ? (
              <div className="p-8 text-center text-gray-500 border border-gray-800 rounded-xl">
                No model calls recorded yet.
              </div>
            ) : (
              modelCalls.map((call) => (
                <div key={call.id} className="p-4 bg-gray-900 border border-gray-800 rounded-xl space-y-3">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center space-x-3">
                      <span className="font-semibold text-white text-sm">
                        Round {call.round}
                      </span>
                      <span className={`text-xs px-2 py-0.5 rounded ${
                        call.status === 'completed' ? 'bg-green-500/10 text-green-400' :
                        call.status === 'failed' ? 'bg-red-500/10 text-red-400' : 'bg-blue-500/10 text-blue-400'
                      }`}>
                        {call.status}
                      </span>
                      {call.finish_reason && (
                        <span className="text-xs font-mono text-gray-400 bg-gray-950 px-2 py-0.5 rounded border border-gray-800">
                          finish: {call.finish_reason}
                        </span>
                      )}
                    </div>
                    <div className="text-xs font-mono text-gray-400 space-x-3">
                      {call.usage_known ? <>
                        <span>input: {call.prompt_tokens}</span>
                        <span>output: {call.completion_tokens}</span>
                        <span className="text-emerald-400">total: {call.total_tokens}</span>
                      </> : <span>用量未报告</span>}
                    </div>
                  </div>
                  {(call.request_id || call.response_id) && (
                    <div className="text-[11px] text-gray-500 font-mono break-all">
                      {call.request_id && <div>Request: {call.request_id}</div>}
                      {call.response_id && <div>Response: {call.response_id}</div>}
                    </div>
                  )}
                  {call.error_message && (
                    <div className="p-2.5 bg-red-500/10 border border-red-500/30 rounded text-red-400 text-xs font-mono">
                      {call.error_message}
                    </div>
                  )}
                  <div className="text-[11px] text-gray-500">
                    Started: {new Date(call.started_at).toLocaleTimeString()}
                    {call.finished_at && ` • Finished: ${new Date(call.finished_at).toLocaleTimeString()}`}
                  </div>
                </div>
              ))
            )}
          </div>
        )}

        {activeTab === 'tool_calls' && (
          <div className="space-y-4">
            {toolCalls.length === 0 ? (
              <div className="p-8 text-center text-gray-500 border border-gray-800 rounded-xl">
                No tool calls recorded yet.
              </div>
            ) : (
              toolCalls.map((tc) => (
                <div key={tc.id} className="p-4 bg-gray-900 border border-gray-800 rounded-xl space-y-3">
                  <div className="flex items-center justify-between">
                    <div className="flex items-center space-x-3">
                      <span className="font-mono font-semibold text-blue-400 text-sm">
                        {tc.tool_name}
                      </span>
                      <span className={`text-xs px-2 py-0.5 rounded ${
                        tc.status === 'success' ? 'bg-green-500/10 text-green-400' :
                        tc.status === 'rejected' ? 'bg-amber-500/10 text-amber-400' : 'bg-red-500/10 text-red-400'
                      }`}>
                        {tc.status}
                      </span>
                    </div>
                    <span className="font-mono text-xs text-gray-500">
                      ID: {tc.call_id}
                    </span>
                  </div>

                  <div>
                    <span className="text-[11px] text-gray-400 font-semibold block mb-1">Arguments:</span>
                    <pre className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-gray-300 overflow-x-auto">
                      {JSON.stringify(tc.arguments || {}, null, 2)}
                    </pre>
                  </div>

                  {tc.output && (
                    <div>
                      {tc.tool_name === 'get_endpoint' ? (() => {
                        const httpData = parseHttpOutput(tc.output);
                        if (!httpData) {
                          return (
                            <div>
                              <span className="text-[11px] text-gray-400 font-semibold block mb-1">Output:</span>
                              <pre className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-emerald-300 overflow-x-auto max-h-60">
                                {tc.output}
                              </pre>
                            </div>
                          );
                        }
                        return (
                          <div className="space-y-3">
                            <span className="text-[11px] text-gray-400 font-semibold block">HTTP Request & Evidence:</span>
                            {/* HTTP Request & Response Status bar */}
                            <div className="p-3 bg-gray-950 rounded-lg border border-gray-800 flex flex-wrap items-center justify-between gap-2">
                              <div className="flex items-center gap-2">
                                <span className="px-2 py-0.5 bg-blue-500/20 text-blue-400 text-xs font-bold font-mono rounded">
                                  {httpData.request?.method || 'GET'}
                                </span>
                                <span className="text-xs font-mono text-gray-200 break-all">
                                  {httpData.request?.url}
                                </span>
                              </div>
                              <div className="flex items-center gap-2">
                                {httpData.response?.status_code && getStatusCodeBadge(httpData.response.status_code)}
                                {httpData.response?.duration_ms !== undefined && (
                                  <span className="text-xs font-mono text-gray-400 bg-gray-900 px-2 py-0.5 rounded border border-gray-800">
                                    {httpData.response.duration_ms} ms
                                  </span>
                                )}
                                {httpData.integrity?.is_truncated && (
                                  <span className="px-2 py-0.5 bg-amber-500/20 text-amber-300 text-[11px] font-semibold rounded border border-amber-500/30 flex items-center gap-1">
                                    <span>⚠️</span>
                                    <span>响应已被截断至 64 KiB</span>
                                  </span>
                                )}
                              </div>
                            </div>

                            {/* Response Headers */}
                            {httpData.response?.headers && Object.keys(httpData.response.headers).length > 0 && (
                              <div>
                                <span className="text-[11px] text-gray-400 font-semibold block mb-1">Response Headers:</span>
                                <div className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-gray-300 space-y-1 max-h-40 overflow-y-auto">
                                  {Object.entries(httpData.response.headers).map(([k, v]) => (
                                    <div key={k} className="flex gap-2">
                                      <span className="text-indigo-400 shrink-0">{k}:</span>
                                      <span className="text-gray-300 break-all">{v}</span>
                                    </div>
                                  ))}
                                </div>
                              </div>
                            )}

                            {/* Response Body Preview */}
                            {httpData.response?.body !== undefined && (
                              <div>
                                <div className="flex items-center justify-between mb-1">
                                  <span className="text-[11px] text-gray-400 font-semibold">Response Body Preview:</span>
                                  <span className="text-[11px] text-gray-500 font-mono">
                                    Saved {httpData.integrity?.saved_bytes ?? 0} bytes
                                  </span>
                                </div>
                                <pre className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-emerald-300 overflow-x-auto max-h-60 whitespace-pre-wrap break-all">
                                  {httpData.response.body || '<empty body>'}
                                </pre>
                              </div>
                            )}

                            {/* Error Info if any */}
                            {httpData.error && (
                              <div className="p-2.5 bg-red-500/10 border border-red-500/30 rounded text-red-400 text-xs font-mono">
                                <span className="font-semibold">[{httpData.error.type}]</span> {httpData.error.message}
                              </div>
                            )}
                          </div>
                        );
                      })() : (
                        <div>
                          <span className="text-[11px] text-gray-400 font-semibold block mb-1">Output:</span>
                          <pre className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-emerald-300 overflow-x-auto max-h-60">
                            {tc.output}
                          </pre>
                        </div>
                      )}
                    </div>
                  )}

                  {tc.error_message && (
                    <div className="p-2.5 bg-red-500/10 border border-red-500/30 rounded text-red-400 text-xs font-mono">
                      {tc.error_message}
                    </div>
                  )}
                </div>
              ))
            )}
          </div>
        )}

        {activeTab === 'messages' && (
          <div className="space-y-4">
            {messages.length === 0 ? (
              <div className="p-8 text-center text-gray-500 border border-gray-800 rounded-xl">
                No conversation messages recorded yet.
              </div>
            ) : (
              messages.map((msg) => (
                <div key={msg.id} className="p-4 bg-gray-900 border border-gray-800 rounded-xl space-y-2">
                  <div className="flex items-center justify-between">
                    <span className={`text-xs font-semibold uppercase px-2 py-0.5 rounded ${
                      msg.role === 'system' ? 'bg-gray-800 text-gray-300' :
                      msg.role === 'user' ? 'bg-blue-500/20 text-blue-300' :
                      msg.role === 'assistant' ? 'bg-purple-500/20 text-purple-300' :
                      'bg-emerald-500/20 text-emerald-300'
                    }`}>
                      {msg.role}
                    </span>
                    <span className="text-xs font-mono text-gray-500">
                      Seq #{msg.seq}
                    </span>
                  </div>
                  {msg.content && (
                    <div className="text-sm text-gray-200 whitespace-pre-wrap font-sans">
                      {msg.content}
                    </div>
                  )}
                  {msg.tool_calls && (
                    <div className="pt-2">
                      <span className="text-[11px] text-gray-400 font-semibold block mb-1">Tool Calls:</span>
                      <pre className="p-2 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-purple-300 overflow-x-auto">
                        {JSON.stringify(msg.tool_calls, null, 2)}
                      </pre>
                    </div>
                  )}
                  {msg.tool_call_id && (
                    <div className="text-xs font-mono text-gray-500">
                      for tool_call_id: {msg.tool_call_id}
                    </div>
                  )}
                </div>
              ))
            )}
          </div>
        )}
      </div>
    </div>
  );
}
