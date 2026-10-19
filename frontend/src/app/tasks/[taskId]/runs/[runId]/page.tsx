'use client';

import React, { useEffect, useState, useCallback, useRef } from 'react';
import { useParams } from 'next/navigation';
import Link from 'next/link';
import { api } from '@/lib/api';
import { Run, RunEvent } from '@/lib/types';
import { RunStatusBadge } from '@/components/RunStatusBadge';
import { EventTimeline } from '@/components/EventTimeline';

export default function RunDetailPage() {
  const params = useParams();
  const taskId = params.taskId as string;
  const runId = params.runId as string;

  const [run, setRun] = useState<Run | null>(null);
  const [events, setEvents] = useState<RunEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  const [stopping, setStopping] = useState(false);
  const [stopError, setStopError] = useState<string | null>(null);
  const [configOpen, setConfigOpen] = useState(false);

  const loadRunData = useCallback(async (isInitial = false) => {
    if (isInitial) setLoading(true);
    setError(null);
    try {
      const [runData, eventsData] = await Promise.all([
        api.getRun(runId),
        api.getRunEvents(runId),
      ]);
      setRun(runData);
      setEvents(eventsData);
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
        const [updatedRun, updatedEvents] = await Promise.all([
          api.getRun(runId),
          api.getRunEvents(runId),
        ]);
        setRun(updatedRun);
        setEvents(updatedEvents);
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
      // Re-fetch events
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

      {/* Run Top Banner */}
      <div className="bg-gray-900 border border-gray-800 rounded-2xl p-6 shadow-sm space-y-6">
        <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-4 pb-6 border-b border-gray-800">
          <div className="space-y-2">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="text-2xl font-bold text-white tracking-tight font-mono">
                Run {run.id}
              </h1>
              {/* Large status badge */}
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
            <span className="text-xs text-gray-500 block mb-1">Fail At Step</span>
            <span className="text-xs sm:text-sm font-mono text-gray-200">
              {run.fail_at_step !== undefined && run.fail_at_step !== null
                ? `Step ${run.fail_at_step}`
                : 'None (Run to completion)'}
            </span>
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
                Configuration Snapshot
              </span>
              <span className="text-xs text-gray-500">
                ({Object.keys(run.config_snapshot || {}).length} keys)
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
            <div className="p-4 border-t border-gray-800 bg-gray-950 overflow-x-auto">
              <pre className="text-xs font-mono text-gray-300">
                {JSON.stringify(run.config_snapshot || {}, null, 2)}
              </pre>
            </div>
          )}
        </div>
      </div>

      {/* Event Timeline Section */}
      <div className="space-y-4">
        <div className="flex items-center justify-between">
          <div className="flex items-center space-x-3">
            <h2 className="text-xl font-bold text-white">Event Timeline</h2>
            <span className="text-xs px-2 py-0.5 rounded-full bg-gray-800 text-gray-400 border border-gray-700">
              {events.length} events
            </span>
          </div>
        </div>

        <EventTimeline events={events} />
      </div>
    </div>
  );
}
