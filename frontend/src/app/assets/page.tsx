'use client';

import React, { useEffect, useState } from 'react';
import Link from 'next/link';
import { api } from '@/lib/api';
import { AssetSummary } from '@/lib/types';

export default function AssetsPage() {
  const [assets, setAssets] = useState<AssetSummary[]>([]);
  const [loading, setLoading] = useState<boolean>(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    async function loadAssets() {
      try {
        setLoading(true);
        const data = await api.getAssets();
        setAssets(data);
      } catch (err: any) {
        setError(err.message || 'Failed to load assets');
      } finally {
        setLoading(false);
      }
    }
    loadAssets();
  }, []);

  return (
    <div className="space-y-6">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center sm:justify-between gap-4 border-b border-gray-800 pb-5">
        <div>
          <h1 className="text-2xl font-bold tracking-tight text-white flex items-center gap-2.5">
            <span className="p-1.5 rounded-lg bg-indigo-500/10 text-indigo-400 border border-indigo-500/20">
              <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
              </svg>
            </span>
            Assets & Targets
          </h1>
          <p className="mt-1 text-sm text-gray-400">
            Persistent entities with stable cross-run identities, tracking historical inspections, work items, and verified observations.
          </p>
        </div>
      </div>

      {/* Error Alert */}
      {error && (
        <div className="p-4 rounded-xl bg-red-500/10 border border-red-500/20 text-red-400 text-sm">
          {error}
        </div>
      )}

      {/* Content */}
      {loading ? (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
          {[1, 2, 3].map((i) => (
            <div key={i} className="h-44 rounded-xl bg-gray-900/60 border border-gray-800 animate-pulse" />
          ))}
        </div>
      ) : assets.length === 0 ? (
        <div className="text-center py-16 px-4 rounded-2xl border border-dashed border-gray-800 bg-gray-900/30">
          <div className="w-12 h-12 mx-auto rounded-full bg-gray-800 flex items-center justify-center text-gray-500 mb-3">
            <svg className="w-6 h-6" fill="none" viewBox="0 0 24 24" stroke="currentColor">
              <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={1.5} d="M20 7l-8-4-8 4m16 0l-8 4m8-4v10l-8 4m0-10L4 7m8 4v10M4 7v10l8 4" />
            </svg>
          </div>
          <h3 className="text-base font-semibold text-gray-300">No assets discovered yet</h3>
          <p className="mt-1 text-sm text-gray-500 max-w-sm mx-auto">
            Assets are automatically registered when runs execute tasks targeting supported endpoints.
          </p>
        </div>
      ) : (
        <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-5">
          {assets.map((asset) => (
            <Link
              key={asset.id}
              href={`/assets/${asset.id}`}
              className="group block p-5 rounded-xl bg-gray-900 border border-gray-800 hover:border-indigo-500/50 hover:bg-gray-850/50 transition-all duration-200"
            >
              <div className="flex items-start justify-between gap-3 mb-3">
                <div className="min-w-0">
                  <span className="inline-flex items-center px-2 py-0.5 rounded text-xs font-medium bg-indigo-500/10 text-indigo-400 border border-indigo-500/20 mb-2">
                    {asset.kind}
                  </span>
                  <h2 className="text-lg font-semibold text-white group-hover:text-indigo-300 transition-colors truncate">
                    {asset.name}
                  </h2>
                  <p className="font-mono text-xs text-gray-500 mt-0.5 truncate">
                    {asset.namespace}:{asset.external_key}
                  </p>
                </div>
              </div>

              <p className="text-sm text-gray-400 line-clamp-2 mb-4 h-10">
                {asset.description || 'No description provided.'}
              </p>

              <div className="pt-3 border-t border-gray-800/80 flex items-center justify-between text-xs text-gray-400">
                <div className="flex items-center gap-1.5" title="Associated Runs">
                  <svg className="w-4 h-4 text-gray-500" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M13 10V3L4 14h7v7l9-11h-7z" />
                  </svg>
                  <span>
                    <strong className="text-gray-200">{asset.run_count}</strong> {asset.run_count === 1 ? 'Run' : 'Runs'}
                  </span>
                </div>

                <div className="flex items-center gap-1.5" title="Recorded Observations">
                  <svg className="w-4 h-4 text-emerald-500" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z" />
                  </svg>
                  <span>
                    <strong className="text-gray-200">{asset.observation_count}</strong> {asset.observation_count === 1 ? 'Obs' : 'Obs'}
                  </span>
                </div>

                <div className="flex items-center gap-1.5" title="Executed Work Items">
                  <svg className="w-4 h-4 text-blue-500" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                    <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2" />
                  </svg>
                  <span>
                    <strong className="text-gray-200">{asset.work_item_count}</strong> Items
                  </span>
                </div>
              </div>
            </Link>
          ))}
        </div>
      )}
    </div>
  );
}
