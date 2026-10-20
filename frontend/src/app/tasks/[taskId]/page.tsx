'use client';

import React, { useEffect, useState, useCallback, useRef } from 'react';
import { useParams } from 'next/navigation';
import Link from 'next/link';
import { api } from '@/lib/api';
import { Task, Run } from '@/lib/types';
import { RunStatusBadge } from '@/components/RunStatusBadge';

export default function TaskDetailPage() {
  const params = useParams();
  const taskId = params.taskId as string;

  const [task, setTask] = useState<Task | null>(null);
  const [runs, setRuns] = useState<Run[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [startingRun, setStartingRun] = useState(false);
  const [startError, setStartError] = useState<string | null>(null);
  const [failAtStep, setFailAtStep] = useState<string>('');

  const loadData = useCallback(async (isInitial = false) => {
    if (isInitial) setLoading(true);
    setError(null);
    try {
      const [taskData, runsData] = await Promise.all([
        api.getTask(taskId),
        api.getRuns(taskId),
      ]);
      setTask(taskData);
      setRuns(runsData);
    } catch (err: unknown) {
      if (err instanceof Error) {
        setError(err.message);
      } else {
        setError('Failed to load task details');
      }
    } finally {
      if (isInitial) setLoading(false);
    }
  }, [taskId]);

  useEffect(() => {
    loadData(true);
  }, [loadData]);

  // Polling logic: poll every 2s if any run is active (queued, running, stopping)
  const isPollingRef = useRef(false);
  useEffect(() => {
    const hasActive = runs.some((r) =>
      ['queued', 'running', 'stopping'].includes(r.status.toLowerCase())
    );

    if (!hasActive) return;

    const interval = setInterval(async () => {
      if (isPollingRef.current) return;
      isPollingRef.current = true;
      try {
        const updatedRuns = await api.getRuns(taskId);
        setRuns(updatedRuns);
      } catch {
        // Polling errors can be silently caught to avoid flashing error banners
      } finally {
        isPollingRef.current = false;
      }
    }, 2000);

    return () => clearInterval(interval);
  }, [runs, taskId]);

  const [executorType, setExecutorType] = useState<'orchestrator' | 'agent' | 'fake'>('orchestrator');
  const [providerType, setProviderType] = useState<'mock' | 'openai'>('mock');
  const [mockScenario, setMockScenario] = useState('http_traceability');
  const [parallelWorkers, setParallelWorkers] = useState<number>(1);

  const handleStartRun = async (e: React.FormEvent) => {
    e.preventDefault();
    setStartingRun(true);
    setStartError(null);

    try {
      let input: any = {};
      if (executorType === 'orchestrator') {
        input = {
          executor: 'orchestrator',
          provider: providerType,
          budget: {
            max_parallel_workers: parallelWorkers,
            max_model_calls: 30,
            max_tool_calls: 30,
          },
        };
        if (providerType === 'mock') {
          input.mock_scenario = mockScenario;
        }
      } else if (executorType === 'agent') {
        input = {
          executor: 'agent',
          provider: providerType,
        };
        if (providerType === 'mock') {
          input.mock_scenario = mockScenario;
        }
      } else {
        const step = failAtStep.trim() !== '' ? parseInt(failAtStep.trim(), 10) : undefined;
        input = {
          executor: 'fake',
          fail_at_step: Number.isInteger(step) ? step : undefined,
        };
      }

      const newRun = await api.createRun(taskId, input);
      setRuns((prev) => [newRun, ...prev.filter((r) => r.id !== newRun.id)]);
      setFailAtStep('');
    } catch (err: unknown) {
      if (err instanceof Error) {
        setStartError(err.message);
      } else {
        setStartError('Failed to start run');
      }
    } finally {
      setStartingRun(false);
    }
  };

  const calculateDuration = (run: Run): string | null => {
    if (!run.finished_at) return null;
    const start = run.started_at
      ? new Date(run.started_at).getTime()
      : new Date(run.created_at).getTime();
    const finish = new Date(run.finished_at).getTime();
    const durationSeconds = Math.max(0, Math.round((finish - start) / 1000));

    if (durationSeconds < 60) {
      return `${durationSeconds}s`;
    }
    const mins = Math.floor(durationSeconds / 60);
    const secs = durationSeconds % 60;
    return `${mins}m ${secs}s`;
  };

  if (loading) {
    return (
      <div className="space-y-6 animate-pulse">
        <div className="h-6 w-32 bg-gray-800 rounded" />
        <div className="h-10 w-2/3 bg-gray-800 rounded" />
        <div className="h-20 w-full bg-gray-900 rounded-xl" />
        <div className="h-64 w-full bg-gray-900 rounded-xl" />
      </div>
    );
  }

  if (error || !task) {
    return (
      <div className="space-y-4">
        <Link
          href="/tasks"
          className="inline-flex items-center text-sm text-gray-400 hover:text-white transition-colors"
        >
          <svg className="w-4 h-4 mr-1" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
          </svg>
          Back to Tasks
        </Link>
        <div className="p-6 bg-red-500/10 border border-red-500/30 rounded-xl text-red-400">
          <h2 className="text-lg font-semibold mb-2">Error Loading Task</h2>
          <p className="text-sm mb-4">{error || 'Task not found'}</p>
          <button
            onClick={() => loadData(true)}
            className="px-4 py-2 bg-red-500/20 hover:bg-red-500/30 text-red-300 rounded-lg text-sm font-medium transition-colors"
          >
            Try Again
          </button>
        </div>
      </div>
    );
  }

  const hasActiveRun = runs.some((r) =>
    ['queued', 'running', 'stopping'].includes(r.status.toLowerCase())
  );

  return (
    <div className="space-y-8">
      {/* Navigation breadcrumb */}
      <div>
        <Link
          href="/tasks"
          className="inline-flex items-center text-sm text-gray-400 hover:text-white transition-colors group"
        >
          <svg
            className="w-4 h-4 mr-1.5 transition-transform group-hover:-translate-x-0.5"
            fill="none"
            viewBox="0 0 24 24"
            stroke="currentColor"
          >
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 19l-7-7 7-7" />
          </svg>
          Back to Tasks
        </Link>
      </div>

      {/* Task Header & Info */}
      <div className="bg-gray-900 border border-gray-800 rounded-2xl p-6 shadow-sm">
        <div className="flex flex-col md:flex-row md:items-start md:justify-between gap-6">
          <div className="space-y-2 max-w-3xl">
            <div className="flex items-center space-x-3">
              <h1 className="text-2xl sm:text-3xl font-bold text-white tracking-tight">
                {task.name}
              </h1>
              <span className="font-mono text-xs px-2 py-0.5 bg-gray-800 text-gray-400 rounded border border-gray-700">
                {task.id.slice(0, 8)}
              </span>
            </div>
            <p className="text-gray-300 text-sm leading-relaxed whitespace-pre-wrap">
              {task.objective || (
                <span className="text-gray-500 italic">No objective provided</span>
              )}
            </p>
            <div className="pt-2 flex items-center gap-4 text-xs text-gray-500">
              <span>
                Created: {new Date(task.created_at).toLocaleString()}
              </span>
              <span>•</span>
              <span>
                Updated: {new Date(task.updated_at).toLocaleString()}
              </span>
            </div>
          </div>

          {/* Start Run Action Form */}
          <div className="bg-gray-950 border border-gray-800/80 rounded-xl p-4 md:w-96 flex-shrink-0">
            <h2 className="text-xs font-semibold uppercase tracking-wider text-gray-400 mb-3">
              Execute Task
            </h2>
            <form onSubmit={handleStartRun} className="space-y-3">
              {/* Executor Type Tabs */}
              <div>
                <label className="block text-xs font-medium text-gray-400 mb-1.5">
                  Execution Mode
                </label>
                <div className="grid grid-cols-3 gap-1 bg-gray-900 p-1 rounded-lg border border-gray-800">
                  <button
                    type="button"
                    onClick={() => {
                      setExecutorType('orchestrator');
                      setMockScenario('orchestration');
                    }}
                    className={`py-1 text-[11px] font-medium rounded-md transition-colors ${
                      executorType === 'orchestrator'
                        ? 'bg-indigo-600 text-white shadow-sm'
                        : 'text-gray-400 hover:text-white'
                    }`}
                  >
                    Orchestrator
                  </button>
                  <button
                    type="button"
                    onClick={() => {
                      setExecutorType('agent');
                      setMockScenario('normal');
                    }}
                    className={`py-1 text-[11px] font-medium rounded-md transition-colors ${
                      executorType === 'agent'
                        ? 'bg-blue-600 text-white shadow-sm'
                        : 'text-gray-400 hover:text-white'
                    }`}
                  >
                    Agent Runner
                  </button>
                  <button
                    type="button"
                    onClick={() => setExecutorType('fake')}
                    className={`py-1 text-[11px] font-medium rounded-md transition-colors ${
                      executorType === 'fake'
                        ? 'bg-blue-600 text-white shadow-sm'
                        : 'text-gray-400 hover:text-white'
                    }`}
                  >
                    Fake Worker
                  </button>
                </div>
              </div>

              {executorType === 'orchestrator' && (
                <div>
                  <label className="block text-xs font-medium text-gray-400 mb-1">
                    Worker 并发数
                  </label>
                  <div className="grid grid-cols-2 gap-1.5 bg-gray-900 p-1 rounded-lg border border-gray-800">
                    <button
                      type="button"
                      onClick={() => setParallelWorkers(1)}
                      className={`py-1 text-xs font-medium rounded-md transition-colors ${
                        parallelWorkers === 1
                          ? 'bg-indigo-600 text-white shadow-sm'
                          : 'text-gray-400 hover:text-white'
                      }`}
                    >
                      单 Worker (串行)
                    </button>
                    <button
                      type="button"
                      onClick={() => setParallelWorkers(2)}
                      className={`py-1 text-xs font-medium rounded-md transition-colors ${
                        parallelWorkers === 2
                          ? 'bg-indigo-600 text-white shadow-sm'
                          : 'text-gray-400 hover:text-white'
                      }`}
                    >
                      双 Worker (并行)
                    </button>
                  </div>
                </div>
              )}

              {executorType === 'agent' || executorType === 'orchestrator' ? (
                <>
                  <div>
                    <label className="block text-xs font-medium text-gray-400 mb-1">
                      Provider
                    </label>
                    <div className="grid grid-cols-2 gap-1.5">
                      <button
                        type="button"
                        onClick={() => setProviderType('mock')}
                        className={`py-1 px-2 text-xs font-medium rounded-lg border transition-colors ${
                          providerType === 'mock'
                            ? 'bg-amber-500/10 border-amber-500/40 text-amber-300'
                            : 'bg-gray-900 border-gray-800 text-gray-400 hover:text-white'
                        }`}
                      >
                        Mock Provider
                      </button>
                      <button
                        type="button"
                        onClick={() => setProviderType('openai')}
                        className={`py-1 px-2 text-xs font-medium rounded-lg border transition-colors ${
                          providerType === 'openai'
                            ? 'bg-blue-500/10 border-blue-500/40 text-blue-300'
                            : 'bg-gray-900 border-gray-800 text-gray-400 hover:text-white'
                        }`}
                      >
                        真实模型 API
                      </button>
                    </div>
                  </div>

                  {providerType === 'mock' ? (
                    <div>
                      <label
                        htmlFor="mock-scenario"
                        className="block text-xs font-medium text-gray-400 mb-1"
                      >
                        Mock Scenario
                      </label>
                      <select
                        id="mock-scenario"
                        value={mockScenario}
                        onChange={(e) => setMockScenario(e.target.value)}
                        className="w-full px-3 py-1.5 bg-gray-900 border border-gray-800 rounded-lg text-xs text-gray-100 focus:outline-none focus:ring-1 focus:ring-blue-500"
                      >
                        <option value="http_traceability">http_traceability (阶段五: 本地 HTTP 读取与真实请求留痕)</option>
                        <option value="traceability">traceability (阶段四: 资产溯源 + 多轮核验 3 Rounds / 5 Items)</option>
                        <option value="orchestration">orchestration (阶段三: Planner + Workers + Summary 2 Rounds / 4 Items)</option>
                        <option value="normal">normal (list_samples → read_sample → complete)</option>
                        <option value="text_and_tool">text_and_tool (cot thought + tool call)</option>
                        <option value="invalid_tool">invalid_tool (calls non-existent tool)</option>
                        <option value="invalid_args">invalid_args (schema parameter mismatch)</option>
                        <option value="infinite_tools">infinite_tools (triggers tool budget exhaustion)</option>
                        <option value="refusal">refusal (model safety refusal)</option>
                        <option value="truncated">truncated (max_tokens cut off)</option>
                        <option value="empty_response">empty_response (empty response error)</option>
                        <option value="provider_error">provider_error (simulated 500 error)</option>
                        <option value="delay">delay (sleeps to test cancellation)</option>
                      </select>
                    </div>
                  ) : (
                    <div className="p-2.5 bg-gray-900/80 border border-gray-800 rounded-lg text-[11px] text-gray-400 leading-relaxed">
                      由后端环境配置连接： <code className="text-gray-300">LLM_API_KEY</code>, <code className="text-gray-300">LLM_BASE_URL</code>, <code className="text-gray-300">LLM_MODEL</code>。<p className="mt-1">通过 <code className="text-gray-300">LLM_API_FORMAT</code> 选择 <code>responses</code> 或 <code>chat_completions</code>（默认）。</p>
                      <p className="text-amber-400/90 mt-1">缺少配置会阻止启动；不会自动切换为 Mock。</p>
                    </div>
                  )}
                </>
              ) : (
                <div>
                  <label
                    htmlFor="fail-at-step"
                    className="block text-xs font-medium text-gray-400 mb-1"
                  >
                    Fail at Step (optional)
                  </label>
                  <input
                    id="fail-at-step"
                    type="number"
                    min="1"
                    max="10"
                    value={failAtStep}
                    onChange={(e) => setFailAtStep(e.target.value)}
                    placeholder="e.g. 5 (for testing failure)"
                    disabled={startingRun}
                    className="w-full px-3 py-1.5 bg-gray-900 border border-gray-800 rounded-lg text-sm text-gray-100 placeholder-gray-600 focus:outline-none focus:ring-1 focus:ring-blue-500 transition-all"
                  />
                </div>
              )}

              <button
                type="submit"
                disabled={startingRun || hasActiveRun}
                title={hasActiveRun ? 'A run is currently active' : 'Start a new run'}
                className="w-full py-2 px-4 bg-blue-600 hover:bg-blue-500 disabled:bg-gray-800 disabled:text-gray-500 text-white text-sm font-medium rounded-lg transition-colors flex items-center justify-center space-x-2 shadow-sm mt-2"
              >
                {startingRun ? (
                  <>
                    <svg className="animate-spin h-4 w-4 text-white" viewBox="0 0 24 24" fill="none">
                      <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                      <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
                    </svg>
                    <span>Starting...</span>
                  </>
                ) : (
                  <>
                    <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M14.752 11.168l-3.197-2.132A1 1 0 0010 9.87v4.263a1 1 0 001.555.832l3.197-2.132a1 1 0 000-1.664z" />
                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
                    </svg>
                    <span>Start Run</span>
                  </>
                )}
              </button>

              {hasActiveRun && (
                <p className="text-[11px] text-amber-400 text-center">
                  An active run is currently executing.
                </p>
              )}
            </form>
          </div>
        </div>

        {/* Start error message */}
        {startError && (
          <div className="mt-4 p-3 bg-red-500/10 border border-red-500/30 text-red-400 rounded-lg text-sm flex items-start space-x-2">
            <svg className="w-5 h-5 flex-shrink-0 text-red-400 mt-0.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
            </svg>
            <span>{startError}</span>
          </div>
        )}
      </div>

      {/* Runs Section */}
      <div className="space-y-4">
        <div className="flex items-center justify-between">
          <div className="flex items-center space-x-3">
            <h2 className="text-xl font-bold text-white">Runs</h2>
            <span className="text-xs px-2 py-0.5 rounded-full bg-gray-800 text-gray-400 border border-gray-700">
              {runs.length}
            </span>
          </div>
          {hasActiveRun && (
            <span className="text-xs text-blue-400 flex items-center space-x-1.5 animate-pulse">
              <span className="w-2 h-2 rounded-full bg-blue-400" />
              <span>Live polling active (2s)</span>
            </span>
          )}
        </div>

        {runs.length === 0 ? (
          <div className="text-center py-12 bg-gray-900/30 border border-gray-800 rounded-xl">
            <svg
              className="w-10 h-10 mx-auto mb-2 text-gray-600"
              fill="none"
              viewBox="0 0 24 24"
              stroke="currentColor"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                strokeWidth={1.5}
                d="M13 10V3L4 14h7v7l9-11h-7z"
              />
            </svg>
            <p className="text-sm font-medium text-gray-300">No execution runs yet</p>
            <p className="text-xs text-gray-500 mt-1">
              Click &quot;Start Run&quot; above to trigger an execution.
            </p>
          </div>
        ) : (
          <div className="divide-y divide-gray-800 border border-gray-800 rounded-xl bg-gray-900/50 overflow-hidden">
            {runs.map((run) => {
              const duration = calculateDuration(run);
              const createdAtFormatted = new Date(run.created_at).toLocaleString();

              return (
                <Link
                  key={run.id}
                  href={`/tasks/${taskId}/runs/${run.id}`}
                  className="flex flex-col sm:flex-row sm:items-center justify-between p-4 hover:bg-gray-850 hover:bg-gray-800/50 transition-colors gap-3 group"
                >
                  <div className="flex items-center space-x-4">
                    <RunStatusBadge status={run.status} finishReason={run.finish_reason} />
                    <div>
                      <div className="flex items-center space-x-2">
                        <span className="font-mono text-sm font-semibold text-white group-hover:text-blue-400 transition-colors">
                          Run {run.id.slice(0, 8)}
                        </span>
                        {run.execution_input?.executor === 'agent' ? (
                          <span className="text-[11px] font-mono text-purple-300 bg-purple-500/10 border border-purple-500/20 px-1.5 py-0.5 rounded">
                            Agent ({String((run.execution_input?.agent_config as any)?.provider || 'agent')})
                          </span>
                        ) : run.execution_input?.executor === 'fake' ? (
                          <span className="text-[11px] font-mono text-gray-400 bg-gray-800 border border-gray-700 px-1.5 py-0.5 rounded">
                            FakeWorker
                          </span>
                        ) : null}
                        {run.fail_at_step !== undefined && run.fail_at_step !== null && (
                          <span className="text-[11px] font-mono text-amber-400/90 bg-amber-500/10 border border-amber-500/20 px-1.5 py-0.2 rounded">
                            fail@{run.fail_at_step}
                          </span>
                        )}
                      </div>
                      <p className="text-xs text-gray-500 mt-0.5">
                        Started: {createdAtFormatted}
                      </p>
                    </div>
                  </div>

                  <div className="flex items-center space-x-6 text-xs text-gray-400 self-end sm:self-center">
                    {duration && (
                      <div className="flex items-center space-x-1 font-mono">
                        <svg className="w-3.5 h-3.5 text-gray-500" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 8v4l3 3m6-3a9 9 0 11-18 0 9 9 0 0118 0z" />
                        </svg>
                        <span>{duration}</span>
                      </div>
                    )}
                    {run.finish_reason && (
                      <span className="text-gray-400 truncate max-w-xs italic" title={run.finish_reason}>
                        {run.finish_reason}
                      </span>
                    )}
                    <span className="text-gray-600 group-hover:text-gray-300 transition-colors">
                      <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
                      </svg>
                    </span>
                  </div>
                </Link>
              );
            })}
          </div>
        )}
      </div>
    </div>
  );
}
