'use client';

import React, { useEffect, useState } from 'react';
import Link from 'next/link';
import { useParams } from 'next/navigation';
import { api } from '@/lib/api';
import { AssetDetailsResponse } from '@/lib/types';
import { RunStatusBadge } from '@/components/RunStatusBadge';

export default function AssetDetailPage() {
  const params = useParams();
  const assetId = params?.assetId as string;

  const [data, setData] = useState<AssetDetailsResponse | null>(null);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);
  const [openEvidence, setOpenEvidence] = useState<Record<string, boolean>>({});

  useEffect(() => {
    if (!assetId) return;

    async function loadDetails() {
      try {
        setLoading(true);
        const res = await api.getAssetDetails(assetId);
        setData(res);
      } catch (err: any) {
        setError(err.message || 'Failed to load asset details');
      } finally {
        setLoading(false);
      }
    }

    loadDetails();
  }, [assetId]);

  const toggleEvidence = (obsId: string) => {
    setOpenEvidence((prev) => ({
      ...prev,
      [obsId]: !prev[obsId],
    }));
  };

  if (loading) {
    return (
      <div className="space-y-6 animate-pulse">
        <div className="h-6 w-32 bg-gray-800 rounded" />
        <div className="h-32 bg-gray-900 border border-gray-800 rounded-2xl" />
        <div className="h-64 bg-gray-900 border border-gray-800 rounded-2xl" />
      </div>
    );
  }

  if (error || !data) {
    return (
      <div className="space-y-4">
        <Link
          href="/assets"
          className="inline-flex items-center text-sm text-gray-400 hover:text-white transition-colors"
        >
          &larr; Back to Assets
        </Link>
        <div className="p-4 rounded-xl bg-red-500/10 border border-red-500/20 text-red-400 text-sm">
          {error || 'Asset not found'}
        </div>
      </div>
    );
  }

  const { asset, run_history } = data;

  return (
    <div className="space-y-8">
      {/* Navigation */}
      <div>
        <Link
          href="/assets"
          className="inline-flex items-center text-sm text-gray-400 hover:text-white transition-colors gap-1.5"
        >
          <svg className="w-4 h-4" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M10 19l-7-7m0 0l7-7m-7 7h18" />
          </svg>
          Back to Assets
        </Link>
      </div>

      {/* Asset Header Card */}
      <div className="p-6 rounded-2xl bg-gray-900 border border-gray-800">
        <div className="flex flex-col md:flex-row md:items-start justify-between gap-4">
          <div className="space-y-2">
            <div className="flex items-center gap-2.5">
              <span className="px-2.5 py-0.5 rounded-full text-xs font-semibold bg-indigo-500/10 text-indigo-400 border border-indigo-500/20">
                {asset.kind}
              </span>
              <span className="font-mono text-xs text-gray-500">
                {asset.namespace}:{asset.external_key}
              </span>
            </div>
            <h1 className="text-2xl font-bold text-white tracking-tight">{asset.name}</h1>
            <p className="text-sm text-gray-400 max-w-3xl">
              {asset.description || 'No description available.'}
            </p>
          </div>

          <div className="flex flex-col md:items-end text-xs text-gray-500 gap-1 font-mono">
            <span>Asset ID: {asset.id}</span>
            <span>Created: {new Date(asset.created_at).toLocaleString()}</span>
          </div>
        </div>
      </div>

      {/* Historical Runs & Observations Traceability */}
      <div className="space-y-6">
        <div className="border-b border-gray-800 pb-3 flex items-center justify-between">
          <h2 className="text-lg font-semibold text-white flex items-center gap-2">
            <span>Run History & Audit Trace</span>
            <span className="px-2 py-0.5 rounded-full text-xs bg-gray-800 text-gray-400">
              {run_history.length} {run_history.length === 1 ? 'Run' : 'Runs'}
            </span>
          </h2>
        </div>

        {run_history.length === 0 ? (
          <div className="text-center py-12 px-4 rounded-xl border border-gray-800/80 bg-gray-900/20 text-gray-500 text-sm">
            No execution runs have targeted this asset yet.
          </div>
        ) : (
          <div className="space-y-8">
            {run_history.map((runGroup) => (
              <div
                key={runGroup.run_id}
                className="rounded-2xl border border-gray-800 bg-gray-900/60 overflow-hidden shadow-sm"
              >
                {/* Run Banner */}
                <div className="p-5 bg-gray-850/80 border-b border-gray-800 flex flex-wrap items-center justify-between gap-4">
                  <div className="space-y-1">
                    <div className="flex items-center gap-3">
                      <Link
                        href={`/tasks/${runGroup.task_id}/runs/${runGroup.run_id}`}
                        className="text-base font-semibold text-indigo-400 hover:text-indigo-300 hover:underline transition-colors flex items-center gap-1.5"
                      >
                        <span>{runGroup.task_name}</span>
                        <svg className="w-4 h-4 opacity-70" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M10 6H6a2 2 0 00-2 2v10a2 2 0 002 2h10a2 2 0 002-2v-4M14 4h6m0 0v6m0-6L10 14" />
                        </svg>
                      </Link>
                      <RunStatusBadge status={runGroup.run_status} finishReason={runGroup.finish_reason} />
                    </div>
                    <div className="font-mono text-xs text-gray-500">
                      Run ID: {runGroup.run_id}
                    </div>
                  </div>

                  <div className="text-xs text-gray-400 flex items-center gap-4">
                    {runGroup.started_at && (
                      <div>
                        Started: <span className="text-gray-300">{new Date(runGroup.started_at).toLocaleTimeString()}</span>
                      </div>
                    )}
                    {runGroup.finished_at && (
                      <div>
                        Finished: <span className="text-gray-300">{new Date(runGroup.finished_at).toLocaleTimeString()}</span>
                      </div>
                    )}
                  </div>
                </div>

                <div className="p-6 space-y-6">
                  {/* Discovery Provenance Section */}
                  {runGroup.discovered_by ? (
                    <div className="p-3.5 rounded-xl bg-indigo-500/10 border border-indigo-500/20 text-xs text-indigo-300 flex flex-wrap items-center justify-between gap-2">
                      <div className="flex flex-wrap items-center gap-1.5">
                        <span className="font-semibold text-indigo-200">🔍 发现来源:</span>
                        <span>通过前置页面</span>
                        <Link
                          href={`/assets/${runGroup.discovered_by.source_asset_id}`}
                          className="font-mono text-indigo-400 font-medium underline hover:text-indigo-200"
                        >
                          {runGroup.discovered_by.source_asset_name || runGroup.discovered_by.source_asset_key || '来源页面'}
                        </Link>
                        <span>响应正文中的链接</span>
                        <code className="bg-indigo-950 px-1.5 py-0.5 rounded text-indigo-200 font-mono font-bold">
                          {runGroup.discovered_by.raw_href}
                        </code>
                        <span>发现并登记</span>
                      </div>
                      <div className="text-[11px] text-gray-400 font-mono">
                        来源 ToolCall: {runGroup.discovered_by.source_tool_call_id.slice(0, 8)}...
                      </div>
                    </div>
                  ) : (
                    <div className="p-3 rounded-xl bg-gray-950/60 border border-gray-800 text-xs text-gray-400 flex items-center gap-2">
                      <span className="text-emerald-400 font-medium">🌱 初始入口:</span>
                      <span>任务配置的种子资产入口 (Seed Endpoint)</span>
                    </div>
                  )}

                  {/* Work Items Section */}
                  {runGroup.work_items && runGroup.work_items.length > 0 && (
                    <div className="space-y-3">
                      <h3 className="text-xs font-bold text-gray-400 uppercase tracking-wider flex items-center gap-1.5">
                        <svg className="w-4 h-4 text-blue-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
                        </svg>
                        Targeted Work Items ({runGroup.work_items.length})
                      </h3>
                      <div className="grid grid-cols-1 md:grid-cols-2 gap-3">
                        {runGroup.work_items.map((wi) => (
                          <div
                            key={wi.id}
                            className="p-3.5 rounded-xl bg-gray-950/60 border border-gray-800/80 space-y-2"
                          >
                            <div className="flex items-center justify-between gap-2">
                              <span className="font-mono text-xs font-semibold text-blue-300">
                                {wi.key}
                              </span>
                              <span
                                className={`text-[10px] font-medium px-2 py-0.5 rounded-full uppercase tracking-wider ${
                                  wi.status === 'completed'
                                    ? 'bg-emerald-500/10 text-emerald-400 border border-emerald-500/20'
                                    : wi.status === 'exhausted'
                                      ? 'bg-orange-500/10 text-orange-300 border border-orange-500/20'
                                    : 'bg-gray-800 text-gray-400'
                                }`}
                              >
                                {wi.status}
                              </span>
                            </div>
                            <p className="text-sm font-medium text-gray-200">{wi.title}</p>
                            <p className="text-xs text-gray-400 line-clamp-1">{wi.objective}</p>

                            {wi.source_observations && wi.source_observations.length > 0 && (
                              <div className="pt-2 border-t border-gray-800/50 text-[11px] text-amber-400/90 flex items-center gap-1">
                                <span>Causal Source:</span>
                                <span className="font-mono bg-amber-500/10 px-1.5 py-0.5 rounded border border-amber-500/20">
                                  {wi.source_observations.map((so) => so.obs_type).join(', ')}
                                </span>
                              </div>
                            )}
                          </div>
                        ))}
                      </div>
                    </div>
                  )}

                  {/* Observations Section with Collapsible ToolCall Evidence */}
                  <div className="space-y-3">
                    <h3 className="text-xs font-bold text-gray-400 uppercase tracking-wider flex items-center gap-1.5">
                      <svg className="w-4 h-4 text-emerald-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                        <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
                      </svg>
                      Verified Observations & Evidence ({runGroup.observations.length})
                    </h3>

                    {runGroup.observations.length === 0 ? (
                      <p className="text-xs text-gray-500 italic">No observations linked to this asset in this run.</p>
                    ) : (
                      <div className="space-y-3">
                        {runGroup.observations.map((obs) => {
                          const isOpen = !!openEvidence[obs.id];
                          return (
                            <div
                              key={obs.id}
                              className="rounded-xl border border-gray-800/80 bg-gray-950/60 overflow-hidden"
                            >
                              <div className="p-4 flex flex-col md:flex-row md:items-start justify-between gap-3">
                                <div className="space-y-1.5 min-w-0">
                                  <div className="flex items-center gap-2">
                                    <span className="px-2 py-0.5 rounded text-xs font-medium bg-emerald-500/10 text-emerald-400 border border-emerald-500/20">
                                      {obs.obs_type}
                                    </span>
                                    <span className="text-xs text-gray-400 font-medium">
                                      {obs.content}
                                    </span>
                                  </div>

                                  {obs.data && Object.keys(obs.data).length > 0 && (
                                    <div className="font-mono text-xs text-gray-400 bg-gray-900/80 p-2 rounded border border-gray-800/50">
                                      {JSON.stringify(obs.data)}
                                    </div>
                                  )}
                                </div>

                                {/* Toggle Raw ToolCall Evidence */}
                                {obs.tool_call && (
                                  <button
                                    onClick={() => toggleEvidence(obs.id)}
                                    className="shrink-0 px-3 py-1.5 rounded-lg text-xs font-medium bg-gray-900 hover:bg-gray-800 text-gray-300 border border-gray-700/60 hover:text-white transition-colors flex items-center gap-1.5"
                                  >
                                    <svg
                                      className={`w-3.5 h-3.5 transform transition-transform ${isOpen ? 'rotate-180' : ''}`}
                                      fill="none"
                                      viewBox="0 0 24 24"
                                      stroke="currentColor"
                                    >
                                      <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 9l-7 7-7-7" />
                                    </svg>
                                    <span>{isOpen ? 'Hide Evidence' : 'Inspect Tool Call'}</span>
                                  </button>
                                )}
                              </div>

                              {/* Collapsible Tool Call Box */}
                              {isOpen && obs.tool_call && (
                                <div className="p-4 bg-gray-900/95 border-t border-gray-800/80 space-y-3 text-xs">
                                  <div className="flex items-center justify-between">
                                    <div className="flex items-center gap-2 font-mono">
                                      <span className="text-indigo-400 font-semibold">{obs.tool_call.tool_name}</span>
                                      <span className="text-gray-500">call_id: {obs.tool_call.call_id}</span>
                                    </div>
                                    <span
                                      className={`px-2 py-0.5 rounded text-[10px] font-medium ${
                                        obs.tool_call.status === 'success'
                                          ? 'bg-emerald-500/10 text-emerald-400'
                                          : 'bg-red-500/10 text-red-400'
                                      }`}
                                    >
                                      {obs.tool_call.status}
                                    </span>
                                  </div>

                                  <div>
                                    <div className="text-gray-500 mb-1 font-semibold">Arguments:</div>
                                    <pre className="p-2.5 rounded bg-gray-950 font-mono text-gray-300 overflow-x-auto text-[11px] border border-gray-800">
                                      {JSON.stringify(obs.tool_call.arguments, null, 2)}
                                    </pre>
                                  </div>

                                  <div>
                                    <div className="text-gray-500 mb-1 font-semibold">Raw Tool Output / Sample Body:</div>
                                    <pre className="p-2.5 rounded bg-gray-950 font-mono text-emerald-400/90 overflow-x-auto text-[11px] border border-gray-800 max-h-48 overflow-y-auto">
                                      {obs.tool_call.output}
                                    </pre>
                                  </div>
                                </div>
                              )}
                            </div>
                          );
                        })}
                      </div>
                    )}
                  </div>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
