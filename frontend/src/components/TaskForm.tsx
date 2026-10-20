'use client';

import React, { useState } from 'react';
import { api } from '@/lib/api';
import { Task } from '@/lib/types';

interface TaskFormProps {
  onSuccess: (task: Task) => void;
  onCancel: () => void;
}

export function TaskForm({ onSuccess, onCancel }: TaskFormProps) {
  const [name, setName] = useState('');
  const [objective, setObjective] = useState('');
  const [targetType, setTargetType] = useState<'discovery' | 'http' | 'samples'>('discovery');
  const [discoveryPreset, setDiscoveryPreset] = useState<'live' | 'fixture' | 'custom'>('live');
  const [customSeedUrl, setCustomSeedUrl] = useState('http://192.168.77.88:7780/');
  const [selectedEndpoints, setSelectedEndpoints] = useState<string[]>(['/health', '/login', '/']);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleTargetTypeChange = (type: 'discovery' | 'http' | 'samples') => {
    setTargetType(type);
    if (type === 'discovery') {
      setSelectedEndpoints(['/']);
    } else if (type === 'http') {
      setSelectedEndpoints(['/health', '/login', '/']);
    } else {
      setSelectedEndpoints(['sample_home', 'sample_login', 'sample_health']);
    }
  };

  const toggleEndpoint = (ep: string) => {
    setSelectedEndpoints((prev) =>
      prev.includes(ep) ? prev.filter((x) => x !== ep) : [...prev, ep]
    );
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!name.trim()) {
      setError('Task name is required');
      return;
    }
    if (targetType !== 'discovery' && selectedEndpoints.length === 0) {
      setError('At least one endpoint target must be selected');
      return;
    }

    setLoading(true);
    setError(null);

    try {
      let scopePayload: Record<string, unknown> = {};
      if (targetType === 'discovery') {
        if (discoveryPreset === 'live') {
          scopePayload = {
            mode: 'discovery',
            seeds: ['http://192.168.77.88:7780/'],
            discovery_policy: {
              origin: 'http://192.168.77.88:7780',
              mode: 'same_origin_html',
              allowed_methods: ['GET'],
              max_pages: 20,
              max_depth: 2,
              request_interval_ms: 500,
            },
          };
        } else if (discoveryPreset === 'fixture') {
          const fixturePort = process.env.NEXT_PUBLIC_FIXTURE_PORT || '29090';
          scopePayload = {
            mode: 'discovery',
            seeds: ['/'],
            discovery_policy: {
              origin: `http://127.0.0.1:${fixturePort}`,
              mode: 'exact_paths',
              allowed_methods: ['GET'],
              allowed_paths: ['/', '/login', '/health'],
              max_pages: 20,
              max_depth: 2,
              request_interval_ms: 500,
            },
          };
        } else {
          try {
            const parsed = new URL(customSeedUrl.trim());
            scopePayload = {
              mode: 'discovery',
              seeds: [parsed.href],
              discovery_policy: {
                origin: parsed.origin,
                mode: 'same_origin_html',
                allowed_methods: ['GET'],
                max_pages: 20,
                max_depth: 2,
                request_interval_ms: 500,
              },
            };
          } catch {
            setError('Invalid custom seed URL: must be valid http URL');
            setLoading(false);
            return;
          }
        }
      } else {
        scopePayload = {
          endpoints: selectedEndpoints,
        };
      }

      const task = await api.createTask({
        name: name.trim(),
        objective: objective.trim(),
        scope: scopePayload,
      });
      onSuccess(task);
    } catch (err: unknown) {
      if (err instanceof Error) {
        setError(err.message);
      } else {
        setError('Failed to create task');
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <form
      onSubmit={handleSubmit}
      className="bg-gray-900 border border-gray-800 rounded-xl p-6 shadow-xl mb-8"
    >
      <div className="flex items-center justify-between pb-4 mb-4 border-b border-gray-800">
        <div>
          <h2 className="text-lg font-semibold text-white">Create New Task</h2>
          <p className="text-xs text-gray-400 mt-0.5">
            Define a task name and optional objective for execution
          </p>
        </div>
        <button
          type="button"
          onClick={onCancel}
          disabled={loading}
          aria-label="Close"
          className="text-gray-400 hover:text-gray-200 transition-colors p-1 rounded-md hover:bg-gray-800"
        >
          <svg className="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M6 18L18 6M6 6l12 12" />
          </svg>
        </button>
      </div>

      {error && (
        <div className="mb-4 p-3 bg-red-500/10 border border-red-500/30 text-red-400 rounded-lg text-sm flex items-start space-x-2">
          <svg className="w-5 h-5 flex-shrink-0 text-red-400 mt-0.5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M12 8v4m0 4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z" />
          </svg>
          <span>{error}</span>
        </div>
      )}

      <div className="space-y-4">
        <div>
          <label htmlFor="task-name" className="block text-sm font-medium text-gray-300 mb-1.5">
            Name <span className="text-red-400">*</span>
          </label>
          <input
            id="task-name"
            type="text"
            required
            value={name}
            onChange={(e) => setName(e.target.value)}
            disabled={loading}
            placeholder="e.g. Summarize incident reports"
            className="w-full px-3.5 py-2.5 bg-gray-950 border border-gray-800 rounded-lg text-gray-100 placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-all"
          />
        </div>

        <div>
          <label htmlFor="task-objective" className="block text-sm font-medium text-gray-300 mb-1.5">
            Objective <span className="text-gray-500 text-xs font-normal">(optional)</span>
          </label>
          <textarea
            id="task-objective"
            rows={2}
            value={objective}
            onChange={(e) => setObjective(e.target.value)}
            disabled={loading}
            placeholder="Describe the objective or steps the agent should accomplish..."
            className="w-full px-3.5 py-2.5 bg-gray-950 border border-gray-800 rounded-lg text-gray-100 placeholder-gray-500 focus:outline-none focus:ring-2 focus:ring-blue-500 focus:border-transparent transition-all"
          />
        </div>

        {/* Target Mode Selection */}
        <div className="p-3.5 bg-gray-950/80 border border-gray-800 rounded-lg space-y-3">
          <label className="block text-xs font-semibold text-gray-300 uppercase tracking-wider">
            Target Execution Mode
          </label>
          <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
            <button
              type="button"
              onClick={() => handleTargetTypeChange('discovery')}
              className={`p-2.5 rounded-lg border text-left transition-all ${
                targetType === 'discovery'
                  ? 'border-indigo-500 bg-indigo-500/10 text-white shadow-sm'
                  : 'border-gray-800 bg-gray-900/60 text-gray-400 hover:border-gray-700'
              }`}
            >
              <div className="font-semibold text-xs flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full bg-indigo-400 animate-pulse"></span>
                从首页发现 (第六阶段)
              </div>
              <div className="text-[11px] text-gray-400 mt-1">
                仅以首页为起点，动态提取链接发现 /login 与 /health
              </div>
            </button>

            <button
              type="button"
              onClick={() => handleTargetTypeChange('http')}
              className={`p-2.5 rounded-lg border text-left transition-all ${
                targetType === 'http'
                  ? 'border-blue-500 bg-blue-500/10 text-white'
                  : 'border-gray-800 bg-gray-900/60 text-gray-400 hover:border-gray-700'
              }`}
            >
              <div className="font-semibold text-xs flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full bg-emerald-400"></span>
                手动 HTTP 端点 (第五阶段)
              </div>
              <div className="text-[11px] text-gray-400 mt-1">
                手工勾选已知端点全量关联
              </div>
            </button>

            <button
              type="button"
              onClick={() => handleTargetTypeChange('samples')}
              className={`p-2.5 rounded-lg border text-left transition-all ${
                targetType === 'samples'
                  ? 'border-blue-500 bg-blue-500/10 text-white'
                  : 'border-gray-800 bg-gray-900/60 text-gray-400 hover:border-gray-700'
              }`}
            >
              <div className="font-semibold text-xs flex items-center gap-1.5">
                <span className="w-2 h-2 rounded-full bg-amber-400"></span>
                内置样本 (第四阶段)
              </div>
              <div className="text-[11px] text-gray-400 mt-1">
                使用内置静态内存样本数据
              </div>
            </button>
          </div>

          {targetType === 'discovery' ? (
            <div className="space-y-3 pt-1 border-t border-gray-800/80">
              <div className="space-y-2">
                <span className="text-xs text-gray-400 font-medium block">
                  选择发现目标靶场 (Discovery Target Preset):
                </span>
                <div className="grid grid-cols-1 sm:grid-cols-3 gap-2">
                  <button
                    type="button"
                    onClick={() => setDiscoveryPreset('live')}
                    className={`p-2.5 rounded-lg border text-left transition-all ${
                      discoveryPreset === 'live'
                        ? 'bg-indigo-600/20 border-indigo-500/60 text-white'
                        : 'bg-gray-900 border-gray-800 text-gray-400 hover:border-gray-700'
                    }`}
                  >
                    <div className="font-semibold text-xs text-indigo-400">启岳智造集团靶场</div>
                    <div className="text-[11px] text-gray-400 font-mono mt-0.5">http://192.168.77.88:7780/</div>
                    <div className="text-[10px] text-gray-500 mt-1">同 origin HTML 自动动态提取</div>
                  </button>

                  <button
                    type="button"
                    onClick={() => setDiscoveryPreset('fixture')}
                    className={`p-2.5 rounded-lg border text-left transition-all ${
                      discoveryPreset === 'fixture'
                        ? 'bg-indigo-600/20 border-indigo-500/60 text-white'
                        : 'bg-gray-900 border-gray-800 text-gray-400 hover:border-gray-700'
                    }`}
                  >
                    <div className="font-semibold text-xs text-emerald-400">本地 Fixture 靶场</div>
                    <div className="text-[11px] text-gray-400 font-mono mt-0.5">http://127.0.0.1:29090/</div>
                    <div className="text-[10px] text-gray-500 mt-1">用于受控与故障自动化回归</div>
                  </button>

                  <button
                    type="button"
                    onClick={() => setDiscoveryPreset('custom')}
                    className={`p-2.5 rounded-lg border text-left transition-all ${
                      discoveryPreset === 'custom'
                        ? 'bg-indigo-600/20 border-indigo-500/60 text-white'
                        : 'bg-gray-900 border-gray-800 text-gray-400 hover:border-gray-700'
                    }`}
                  >
                    <div className="font-semibold text-xs text-amber-400">自定义目标种子</div>
                    <div className="text-[11px] text-gray-400 font-mono mt-0.5">输入自定义 HTTP URL</div>
                    <div className="text-[10px] text-gray-500 mt-1">严格同 origin + 深度与条数限制</div>
                  </button>
                </div>

                {discoveryPreset === 'custom' && (
                  <div className="pt-1">
                    <label className="text-xs text-gray-400 block mb-1">自定义种子 URL:</label>
                    <input
                      type="url"
                      value={customSeedUrl}
                      onChange={(e) => setCustomSeedUrl(e.target.value)}
                      placeholder="http://192.168.77.88:7780/"
                      className="w-full bg-gray-950 border border-gray-800 rounded px-3 py-1.5 text-xs text-gray-200 font-mono focus:border-indigo-500 focus:outline-none"
                    />
                  </div>
                )}
              </div>

              <div className="grid grid-cols-1 sm:grid-cols-2 gap-2 text-xs">
                <div className="bg-gray-900/80 p-2.5 rounded border border-gray-800">
                  <div className="text-gray-400 font-semibold mb-1 flex items-center gap-1">
                    <span className="text-indigo-400">●</span> 初始种子资产 (Seeds):
                  </div>
                  <div className="font-mono text-emerald-400 bg-gray-950 px-2 py-1 rounded border border-gray-800 inline-block break-all">
                    {discoveryPreset === 'live'
                      ? 'http://192.168.77.88:7780/ (首页)'
                      : discoveryPreset === 'fixture'
                      ? '/ (本地 fixture 首页)'
                      : customSeedUrl || '未指定'}
                  </div>
                  <p className="text-[11px] text-gray-500 mt-1.5">
                    创建 Run 时初始仅包含首页种子；全局已有资产不会预填入本次 RunAsset。
                  </p>
                </div>

                <div className="bg-gray-900/80 p-2.5 rounded border border-gray-800">
                  <div className="text-gray-400 font-semibold mb-1 flex items-center gap-1">
                    <span className="text-indigo-400">■</span> 允许发现策略 (Discovery Policy):
                  </div>
                  <div className="space-y-1 text-[11px] text-gray-300 font-mono">
                    <div>
                      Origin:{' '}
                      <span className="text-gray-400">
                        {discoveryPreset === 'live'
                          ? 'http://192.168.77.88:7780'
                          : discoveryPreset === 'fixture'
                          ? 'http://127.0.0.1:29090'
                          : customSeedUrl.startsWith('http')
                          ? new URL(customSeedUrl).origin
                          : 'http://...'}
                      </span>
                    </div>
                    <div>
                      Mode:{' '}
                      <span className="text-gray-400">
                        {discoveryPreset === 'fixture' ? 'exact_paths (/, /login, /health)' : 'same_origin_html (最大20页, 深度<=2, 并发=1, 间隔>=500ms)'}
                      </span>
                    </div>
                    <div>Method: <span className="text-gray-400">GET</span></div>
                  </div>
                  <p className="text-[11px] text-gray-500 mt-1">
                    跨 origin、带 query/fragment、二进制文件下载等一律在发包前拦截拒绝并记录原因。
                  </p>
                </div>
              </div>
            </div>
          ) : (
            <div>
              <span className="text-xs text-gray-400 font-medium block mb-1.5">
                Target Endpoints in Scope:
              </span>
              <div className="flex flex-wrap gap-2">
                {targetType === 'http' ? (
                  <>
                    {['/', '/login', '/health'].map((ep) => (
                      <label
                        key={ep}
                        className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded text-xs font-mono cursor-pointer border transition-colors ${
                          selectedEndpoints.includes(ep)
                            ? 'bg-blue-500/20 text-blue-300 border-blue-500/40'
                            : 'bg-gray-900 text-gray-400 border-gray-800 hover:border-gray-700'
                        }`}
                      >
                        <input
                          type="checkbox"
                          checked={selectedEndpoints.includes(ep)}
                          onChange={() => toggleEndpoint(ep)}
                          className="rounded bg-gray-950 border-gray-700 text-blue-600 focus:ring-0"
                        />
                        <span>{ep}</span>
                      </label>
                    ))}
                  </>
                ) : (
                  <>
                    {['sample_home', 'sample_login', 'sample_health'].map((ep) => (
                      <label
                        key={ep}
                        className={`inline-flex items-center gap-1.5 px-2.5 py-1 rounded text-xs font-mono cursor-pointer border transition-colors ${
                          selectedEndpoints.includes(ep)
                            ? 'bg-blue-500/20 text-blue-300 border-blue-500/40'
                            : 'bg-gray-900 text-gray-400 border-gray-800 hover:border-gray-700'
                        }`}
                      >
                        <input
                          type="checkbox"
                          checked={selectedEndpoints.includes(ep)}
                          onChange={() => toggleEndpoint(ep)}
                          className="rounded bg-gray-950 border-gray-700 text-blue-600 focus:ring-0"
                        />
                        <span>{ep}</span>
                      </label>
                    ))}
                  </>
                )}
              </div>
            </div>
          )}
        </div>

        <div className="flex items-center justify-end space-x-3 pt-2">
          <button
            type="button"
            onClick={onCancel}
            disabled={loading}
            className="px-4 py-2 text-sm font-medium text-gray-300 hover:text-white bg-gray-800 hover:bg-gray-750 border border-gray-700/60 rounded-lg transition-colors disabled:opacity-50"
          >
            Cancel
          </button>
          <button
            type="submit"
            disabled={loading || !name.trim()}
            className="px-4 py-2 text-sm font-medium text-white bg-blue-600 hover:bg-blue-500 rounded-lg transition-colors disabled:opacity-50 flex items-center space-x-2"
          >
            {loading && (
              <svg className="animate-spin h-4 w-4 text-white" viewBox="0 0 24 24" fill="none">
                <circle className="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" strokeWidth="4" />
                <path className="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v8H4z" />
              </svg>
            )}
            <span>{loading ? 'Creating...' : 'Create Task'}</span>
          </button>
        </div>
      </div>
    </form>
  );
}

export default TaskForm;
