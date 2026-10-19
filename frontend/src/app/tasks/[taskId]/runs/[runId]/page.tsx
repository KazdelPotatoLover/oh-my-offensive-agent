'use client';

import React, { useEffect, useState, useCallback, useRef } from 'react';
import { useParams } from 'next/navigation';
import Link from 'next/link';
import { api } from '@/lib/api';
import { Run, RunEvent, AgentMessage, ModelCall, AgentToolCall } from '@/lib/types';
import { RunStatusBadge } from '@/components/RunStatusBadge';
import { EventTimeline } from '@/components/EventTimeline';

export default function RunDetailPage() {
  const params = useParams();
  const taskId = params.taskId as string;
  const runId = params.runId as string;

  const [run, setRun] = useState<Run | null>(null);
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [messages, setMessages] = useState<AgentMessage[]>([]);
  const [modelCalls, setModelCalls] = useState<ModelCall[]>([]);
  const [toolCalls, setToolCalls] = useState<AgentToolCall[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState<string | null>(null);
  const [configOpen, setConfigOpen] = useState(false);
  const [activeTab, setActiveTab] = useState<'timeline' | 'model_calls' | 'tool_calls' | 'messages'>('timeline');

  const loadRunData = useCallback(async (isInitial = false) => {
    if (isInitial) setLoading(true);
    setError(null);
    try {
      const [runData, eventsData, messagesData, modelCallsData, toolCallsData] = await Promise.all([
        api.getRun(runId),
        api.getRunEvents(runId),
        api.getRunMessages(runId).catch(() => []),
        api.getRunModelCalls(runId).catch(() => []),
        api.getRunToolCalls(runId).catch(() => []),
      ]);
      setRun(runData);
      setEvents(eventsData);
      setMessages(messagesData);
      setModelCalls(modelCallsData);
      setToolCalls(toolCallsData);
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

  // Polling every 2s if run is active (queued, running, stopping)
  const isPollingRef = useRef(false);
  useEffect(() => {
    if (!run) return;
    const isActive = ['queued', 'running', 'stopping'].includes(
      run.status.toLowerCase()
    );

    if (!isActive) return;

    const interval = setInterval(async () => {
      if (isPollingRef.current) return;
      isPollingRef.current = true;
      try {
        const [updatedRun, updatedEvents, updatedMessages, updatedModelCalls, updatedToolCalls] = await Promise.all([
          api.getRun(runId),
          api.getRunEvents(runId),
          api.getRunMessages(runId).catch(() => []),
          api.getRunModelCalls(runId).catch(() => []),
          api.getRunToolCalls(runId).catch(() => []),
        ]);
        setRun(updatedRun);
        setEvents(updatedEvents);
        setMessages(updatedMessages);
        setModelCalls(updatedModelCalls);
        setToolCalls(updatedToolCalls);
      } catch {
        // Suppress polling error to avoid UI flicker
      } finally {
        isPollingRef.current = false;
      }
    }, 2000);

    return () => clearInterval(interval);
  }, [run, runId]);

  const handleStopRun = async () => {
    if (!run || run.status !== 'running') return;

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
    }
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
  const isAgentRun = run.execution_input?.executor === 'agent' || modelCalls.length > 0 || toolCalls.length > 0;
  const isMockProvider = agentConfig?.provider === 'mock';
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
                  OpenAI Real Model
                </span>
                <span className="text-sm">
                  使用 OpenAI 兼容协议与大语言模型通信
                </span>
              </div>
              {totalTokens > 0 && (
                <span className="text-xs font-mono text-blue-200">
                  消耗 Token 总计: {totalTokens.toLocaleString()}
                </span>
              )}
            </div>
          )}
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
              <RunStatusBadge status={run.status} size="lg" />
            </div>
            {isActive && (
              <p className="text-xs text-blue-400 flex items-center space-x-1.5 animate-pulse">
                <span className="w-2 h-2 rounded-full bg-blue-400" />
                <span>Execution in progress (polling live events every 2s)</span>
              </p>
            )}
          </div>

          {/* Action: Stop Run */}
          {run.status === 'running' && (
            <div>
              <button
                type="button"
                onClick={handleStopRun}
                disabled={stopping}
                className="px-4 py-2 bg-red-600/90 hover:bg-red-500 text-white text-sm font-medium rounded-lg transition-colors flex items-center space-x-2 shadow-sm disabled:opacity-50"
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
              {isAgentRun ? 'Agent Runner' : 'Fake Worker'}
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

      {/* Tabs for Timeline / Agent Details */}
      <div className="space-y-4">
        <div className="flex border-b border-gray-800 space-x-6">
          <button
            type="button"
            onClick={() => setActiveTab('timeline')}
            className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 ${
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
                className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 ${
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
                className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 ${
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
                className={`pb-3 text-sm font-medium border-b-2 transition-colors flex items-center space-x-2 ${
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
                      <span>prompt: {call.prompt_tokens}</span>
                      <span>completion: {call.completion_tokens}</span>
                      <span className="text-emerald-400">total: {call.total_tokens}</span>
                    </div>
                  </div>
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
                      <span className="text-[11px] text-gray-400 font-semibold block mb-1">Output:</span>
                      <pre className="p-2.5 bg-gray-950 border border-gray-800 rounded text-xs font-mono text-emerald-300 overflow-x-auto max-h-60">
                        {tc.output}
                      </pre>
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
