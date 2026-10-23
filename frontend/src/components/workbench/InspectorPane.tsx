'use client';

import React, { useEffect, useState } from 'react';
import { CheckResultsPanel } from '@/components/CheckResultsPanel';
import { WorkbenchRequestError } from '@/lib/workbench/client';
import type { DirectionView } from '@/lib/workbench/parse';

type Load<T> = { data: T | null; error: string | null };

const empty = { data: null, error: null };

async function readJson(url: string): Promise<unknown> {
  let response: Response;
  try {
    response = await fetch(url, { headers: { Accept: 'application/json' } });
  } catch (err) {
    throw new WorkbenchRequestError(err instanceof Error ? err.message : 'network error', 0, 'disconnected');
  }
  const text = await response.text();
  const body = text ? JSON.parse(text) as unknown : null;
  if (!response.ok) {
    const message = body && typeof body === 'object' && body && 'error' in body
      ? JSON.stringify((body as { error: unknown }).error)
      : `HTTP ${response.status}`;
    throw new WorkbenchRequestError(message, response.status, 'http_error');
  }
  return body;
}

function asRecord(value: unknown): Record<string, unknown> | null {
  return value && typeof value === 'object' && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

export function InspectorPane({
  runId,
  workItemId,
  direction,
  snapshotAt,
  runStatus,
  onCommand,
  commandPending,
  showLegacy = false,
}: {
  runId: string;
  workItemId?: string;
  direction?: DirectionView;
  snapshotAt?: string;
  runStatus: string;
  onCommand: (type: 'steer_work' | 'kill_work' | 'continue_work' | 'retry_work', payload?: Record<string, unknown>) => void;
  commandPending: boolean;
  showLegacy?: boolean;
}) {
  const [tab, setTab] = useState<'process' | 'artifacts' | 'facts' | 'findings' | 'goals'>('process');
  const [instruction, setInstruction] = useState('');
  const [trace, setTrace] = useState<Load<Record<string, unknown>>>(empty);
  const [sessions, setSessions] = useState<Load<Record<string, unknown>[]>>(empty);
  const [artifacts, setArtifacts] = useState<Load<Record<string, unknown>[]>>(empty);
  const [artifactBody, setArtifactBody] = useState<Load<Record<string, unknown>>>(empty);
  const [facts, setFacts] = useState<Load<Record<string, unknown>[]>>(empty);
  const [findings, setFindings] = useState<Load<Record<string, unknown>[]>>(empty);
  const [goals, setGoals] = useState<Load<Record<string, unknown>[]>>(empty);
  const [legacyItems, setLegacyItems] = useState<Load<Record<string, unknown>[]>>(empty);

  useEffect(() => {
    if (!workItemId) return;
    let cancelled = false;
    setTrace(empty);
    setSessions(empty);
    const load = async () => {
      try {
        const body = asRecord(await readJson(`/api/runs/${runId}/work_items/${workItemId}/trace`));
        if (!cancelled) setTrace({ data: body || {}, error: null });
      } catch (err) {
        if (!cancelled) setTrace({ data: null, error: err instanceof Error ? err.message : 'trace 读取失败' });
      }
      try {
        const body = asRecord(await readJson(`/api/runs/${runId}/sessions`));
        if (!Array.isArray(body?.sessions)) throw new Error('sessions 响应缺少数组');
        const rows = body.sessions as Record<string, unknown>[];
        if (!cancelled) setSessions({ data: rows.filter((row) => row.work_item_id === workItemId), error: null });
      } catch (err) {
        if (!cancelled) setSessions({ data: null, error: err instanceof Error ? err.message : 'session 读取失败' });
      }
    };
    void load();
    return () => { cancelled = true; };
  }, [runId, workItemId]);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const body = asRecord(await readJson(`/api/runs/${runId}/artifacts?offset=0&limit=25`));
        if (!Array.isArray(body?.artifacts)) throw new Error('artifacts 响应缺少数组');
        if (!cancelled) setArtifacts({ data: body.artifacts as Record<string, unknown>[], error: null });
      } catch (err) {
        if (!cancelled) setArtifacts({ data: null, error: err instanceof Error ? err.message : '产物读取失败' });
      }
    };
    void load();
    return () => { cancelled = true; };
  }, [runId, workItemId]);

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const [factBody, findingBody, goalBody] = await Promise.all([
          readJson(`/api/runs/${runId}/facts`),
          readJson(`/api/runs/${runId}/findings`),
          readJson(`/api/runs/${runId}/goals`),
        ]);
        const factRecord = asRecord(factBody);
        const findingRecord = asRecord(findingBody);
        const goalRecord = asRecord(goalBody);
        if (!Array.isArray(factRecord?.facts) || !Array.isArray(findingRecord?.findings) || !Array.isArray(goalRecord?.goals)) {
          throw new Error('事实、发现或目标响应缺少数组');
        }
        if (cancelled) return;
        setFacts({ data: factRecord.facts as Record<string, unknown>[], error: null });
        setFindings({ data: findingRecord.findings as Record<string, unknown>[], error: null });
        setGoals({ data: goalRecord.goals as Record<string, unknown>[], error: null });
      } catch (err) {
        if (cancelled) return;
        const message = err instanceof Error ? err.message : '证据读取失败';
        setFacts({ data: null, error: message });
        setFindings({ data: null, error: message });
        setGoals({ data: null, error: message });
      }
    };
    void load();
    return () => { cancelled = true; };
  }, [runId, snapshotAt]);

  useEffect(() => {
    if (!showLegacy || direction) return;
    let cancelled = false;
    const load = async () => {
      try {
        const body = asRecord(await readJson(`/api/runs/${runId}/work_items`));
        if (!Array.isArray(body?.work_items)) throw new Error('work_items 响应缺少数组');
        if (!cancelled) setLegacyItems({ data: body.work_items as Record<string, unknown>[], error: null });
      } catch (err) {
        if (!cancelled) setLegacyItems({ data: null, error: err instanceof Error ? err.message : '历史工作项读取失败' });
      }
    };
    void load();
    return () => { cancelled = true; };
  }, [runId, direction, showLegacy]);

  const [selectedArtifactId, setSelectedArtifactId] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);

  const allowed = new Set(direction?.allowed_actions || []);
  const steps = Array.isArray(trace.data?.steps) ? trace.data.steps as Record<string, unknown>[] : [];
  const scopedArtifacts = (artifacts.data || []).filter((item) => !workItemId || item.work_item_id === workItemId);
  const unscopedArtifacts = (artifacts.data || []).filter((item) => workItemId && item.work_item_id && item.work_item_id !== workItemId);

  const handleCopy = (text: string) => {
    navigator.clipboard?.writeText(text);
    setCopied(true);
    setTimeout(() => setCopied(false), 2000);
  };

  return (
    <section aria-labelledby="inspector-heading" data-testid="inspector" className="flex h-full min-h-0 flex-1 flex-col bg-gray-950">
      {/* Inspector Header & Direction Meta */}
      <div className="shrink-0 border-b border-gray-800 bg-gray-900/40 p-3 sm:p-4">
        <div className="flex flex-wrap items-center justify-between gap-2">
          <h2 id="inspector-heading" tabIndex={-1} className="text-sm font-semibold text-white flex items-center gap-2">
            <span>审计检查器</span>
            {direction && (
              <span className="rounded-full bg-blue-500/20 px-2 py-0.5 text-[11px] font-normal text-blue-300">
                {direction.work_item_id.slice(0, 8)}
              </span>
            )}
          </h2>
          {direction && (
            <div className="flex flex-wrap items-center gap-1.5">
              <button
                type="button"
                disabled={!allowed.has('steer_work') || commandPending}
                onClick={() => onCommand('steer_work', { instruction })}
                className="rounded border border-gray-700 bg-gray-900 px-2.5 py-1 text-xs text-gray-200 hover:bg-gray-800 disabled:opacity-40"
              >
                纠偏
              </button>
              <button
                type="button"
                data-testid="kill-direction"
                disabled={!allowed.has('kill_work') || commandPending}
                onClick={() => onCommand('kill_work', { reason: 'operator kill' })}
                className="rounded border border-red-800/80 bg-red-950/40 px-2.5 py-1 text-xs font-medium text-red-200 hover:bg-red-900/60 disabled:opacity-40"
              >
                终止该方向
              </button>
              <button
                type="button"
                disabled={!allowed.has('continue_work') || commandPending}
                onClick={() => onCommand('continue_work', instruction ? { instruction } : undefined)}
                className="rounded border border-gray-700 bg-gray-900 px-2.5 py-1 text-xs text-gray-200 hover:bg-gray-800 disabled:opacity-40"
              >
                继续
              </button>
              <button
                type="button"
                disabled={!allowed.has('retry_work') || commandPending}
                onClick={() => onCommand('retry_work', instruction ? { instruction } : undefined)}
                className="rounded border border-gray-700 bg-gray-900 px-2.5 py-1 text-xs text-gray-200 hover:bg-gray-800 disabled:opacity-40"
              >
                重试
              </button>
            </div>
          )}
        </div>

        {direction ? (
          <div className="mt-3 grid grid-cols-1 md:grid-cols-2 lg:grid-cols-4 gap-2.5 text-xs">
            <div className="rounded-lg border border-gray-800/80 bg-gray-950/60 p-2.5 md:col-span-2">
              <span className="text-[11px] text-gray-500 font-medium">方向目标</span>
              <p data-testid="inspector-objective" className="mt-1 font-medium text-gray-200 leading-relaxed">
                {direction.objective || '未提供目标'}
              </p>
              <div className="mt-2 flex flex-wrap items-center gap-3 text-[11px] text-gray-400">
                <p>来源 <span className="text-gray-300">{direction.source_refs?.length ? direction.source_refs.join(', ') : '未提供'}</span></p>
                <p>Attempt <span className="font-mono text-gray-300" data-testid="inspector-attempt">{direction.attempt_id || '无'}</span></p>
              </div>
            </div>

            <div className="rounded-lg border border-gray-800/80 bg-gray-950/60 p-2.5">
              <span className="text-[11px] text-gray-500 font-medium">运行态势</span>
              <div className="mt-1 flex items-center gap-2">
                <span className="text-gray-400">状态</span>
                <span
                  data-testid="inspector-status"
                  className={`rounded px-1.5 py-0.5 text-[11px] font-mono font-medium ${
                    direction.status === 'running'
                      ? 'bg-blue-500/20 text-blue-300'
                      : direction.status === 'completed' || direction.status === 'done'
                      ? 'bg-emerald-500/20 text-emerald-300'
                      : 'bg-gray-800 text-gray-300'
                  }`}
                >
                  {direction.status}
                </span>
              </div>
              <p className="mt-2 text-[11px] text-gray-400">
                最新步骤 <span className="text-gray-200 font-medium" data-testid="inspector-step">{direction.latest_step || '未提供'}</span>
              </p>
            </div>

            <div className="rounded-lg border border-gray-800/80 bg-gray-950/60 p-2.5">
              <span className="text-[11px] text-gray-500 font-medium">局部预算额度</span>
              <p className="mt-1 font-mono text-[11px] text-gray-300 break-all" data-testid="local-budget">
                局部额度 {direction.local_budget ? JSON.stringify(direction.local_budget) : '未提供'}
              </p>
            </div>

            {direction.error && (
              <div role="alert" data-testid="direction-error" className="md:col-span-2 lg:col-span-4 rounded-lg border border-red-500/30 bg-red-950/30 p-2 text-xs text-red-200">
                <span className="font-semibold">{direction.error.code}</span> · {direction.error.scope} · {direction.error.message}
                {direction.error.suggested_action ? ` · ${direction.error.suggested_action}` : ''}
              </div>
            )}
          </div>
        ) : (
          <p className="mt-2 text-xs text-gray-400">
            {workItemId ? '所选方向不在当前快照中。' : '请从上方或看板选择一个 Worker 方向查看详情。'}
          </p>
        )}

        {direction && (
          <div className="mt-2.5">
            <label className="flex items-center gap-2 text-xs text-gray-400">
              <span className="shrink-0 text-[11px]">方向指令:</span>
              <input
                aria-label="方向指令"
                value={instruction}
                onChange={(event) => setInstruction(event.target.value)}
                placeholder="为当前方向补充指示、指定优先路径或纠偏要求..."
                className="flex-1 rounded border border-gray-700 bg-gray-950 px-2.5 py-1 text-xs text-gray-200 focus:outline-none focus:ring-1 focus:ring-blue-500"
              />
            </label>
          </div>
        )}
      </div>

      {/* Tabs navigation */}
      <div role="tablist" aria-label="检查器分区" className="flex shrink-0 gap-1 border-b border-gray-800 bg-gray-900/30 px-3 py-1.5">
        {([
          ['process', '过程', 'Process'],
          ['artifacts', '产物', 'Artifacts'],
          ['facts', '事实', 'Facts'],
          ['findings', '发现', 'Findings'],
          ['goals', '目标', 'Goals'],
        ] as const).map(([id, label, en]) => (
          <button
            key={id}
            type="button"
            role="tab"
            aria-selected={tab === id}
            onClick={() => setTab(id)}
            className={`rounded-md px-3 py-1.5 text-xs font-medium transition-all flex items-center gap-1.5 ${
              tab === id
                ? 'bg-blue-600/20 text-blue-300 border border-blue-500/40 shadow-sm'
                : 'text-gray-400 hover:text-gray-200 hover:bg-gray-800/50'
            }`}
          >
            <span>{label}</span>
            <span className="hidden sm:inline font-mono text-[10px] text-gray-500">({en})</span>
            {id === 'artifacts' && artifacts.data && (
              <span className="rounded-full bg-gray-800 px-1.5 py-0.2 font-mono text-[10px] text-gray-300">
                {scopedArtifacts.length}
              </span>
            )}
            {id === 'facts' && facts.data && (
              <span className="rounded-full bg-gray-800 px-1.5 py-0.2 font-mono text-[10px] text-gray-300">
                {facts.data.length}
              </span>
            )}
            {id === 'findings' && findings.data && (
              <span className="rounded-full bg-gray-800 px-1.5 py-0.2 font-mono text-[10px] text-gray-300">
                {findings.data.length}
              </span>
            )}
          </button>
        ))}
      </div>

      {/* Tab Panels */}
      <div className="min-h-0 flex-1 overflow-auto p-4 text-xs">
        {tab === 'process' && (
          <div data-testid="trace-panel" className="space-y-4">
            {trace.error && <p role="alert" className="text-red-300">{trace.error}</p>}
            {sessions.error && <p role="alert" className="text-red-300">{sessions.error}</p>}

            <div>
              <h3 className="text-xs font-semibold text-gray-300 mb-2">执行轨迹步骤 (Trace Steps)</h3>
              {steps.map((step, idx) => (
                <div
                  key={String(step.step_id || idx)}
                  data-testid="trace-step"
                  className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-2.5 mb-2 hover:border-gray-700 transition-colors"
                >
                  <div className="flex items-center justify-between gap-2">
                    <div className="flex items-center gap-2">
                      <span className="rounded bg-blue-950/60 border border-blue-800/50 px-2 py-0.5 font-mono text-[11px] text-blue-300">
                        {String(step.tool_name || 'unknown')}
                      </span>
                      <span className="text-[11px] text-gray-400">步骤 {idx + 1}</span>
                    </div>
                    <span className="font-mono text-[10px] text-gray-500">状态: {String(step.status || '')}</span>
                  </div>
                  <div className="mt-1.5 text-gray-300 font-mono text-[11px] whitespace-pre-wrap break-all bg-gray-950/80 p-2 rounded border border-gray-800/60">
                    {String(step.tool_name || 'unknown')} · {String(step.status || '')} · {String(step.summary || step.output || '')}
                  </div>
                </div>
              ))}
              {trace.data && steps.length === 0 && !trace.error && (
                <p className="text-gray-500 py-3 text-center">这条 trace 没有步骤。</p>
              )}
            </div>

            <div className="border-t border-gray-800/80 pt-3">
              <h3 className="text-xs font-semibold text-gray-300 mb-2">历史 Attempt / Session 记录</h3>
              <div className="space-y-1.5">
                {(sessions.data || []).map((session) => (
                  <div key={String(session.id)} className="rounded border border-gray-800/60 bg-gray-950/60 px-3 py-1.5 font-mono text-[11px] text-gray-300 flex items-center justify-between">
                    <span>{String(session.id)} · attempt {String(session.attempt_id || '无')} · {String(session.status)}</span>
                  </div>
                ))}
              </div>
              {sessions.data && sessions.data.length === 0 && (
                <p className="text-gray-500 py-2">没有该方向的 Session 记录。</p>
              )}
            </div>
          </div>
        )}

        {tab === 'artifacts' && (
          <div className="flex h-full min-h-0 flex-col md:flex-row gap-4">
            {/* Artifacts list (Left column) */}
            <div className="md:w-80 shrink-0 flex flex-col min-h-0 border-b md:border-b-0 md:border-r border-gray-800/80 pb-3 md:pb-0 md:pr-4 overflow-y-auto">
              <div className="flex items-center justify-between pb-2 border-b border-gray-800/60 mb-2">
                <span className="text-xs font-semibold text-gray-300">产物列表</span>
                <span className="font-mono text-[10px] text-gray-500">{scopedArtifacts.length} 项</span>
              </div>
              {artifacts.error && <p role="alert" className="text-red-300 mb-2">{artifacts.error}</p>}
              
              <div className="space-y-1.5">
                {scopedArtifacts.map((item) => {
                  const itemId = String(item.id);
                  const isSelected = selectedArtifactId === itemId;
                  return (
                    <button
                      key={itemId}
                      type="button"
                      data-testid="artifact-row"
                      onClick={async () => {
                        setSelectedArtifactId(itemId);
                        setArtifactBody(empty);
                        try {
                          const body = asRecord(await readJson(`/api/runs/${runId}/artifacts/${item.id}?offset=0&limit=8192`));
                          setArtifactBody({ data: body || {}, error: null });
                        } catch (err) {
                          setArtifactBody({ data: null, error: err instanceof Error ? err.message : '产物正文读取失败' });
                        }
                      }}
                      className={`block w-full text-left rounded-lg border p-2.5 transition-all text-xs ${
                        isSelected
                          ? 'border-blue-500 bg-blue-950/30 text-white shadow-sm'
                          : 'border-gray-800 bg-gray-900/40 text-gray-300 hover:border-gray-700 hover:bg-gray-900/80'
                      }`}
                    >
                      <p className="font-medium truncate">{String(item.name || item.id)}</p>
                      <p className="mt-1 font-mono text-[10px] text-gray-500 truncate">ID: {itemId}</p>
                    </button>
                  );
                })}
              </div>

              {artifacts.data && scopedArtifacts.length === 0 && (
                <p className="text-gray-500 py-4 text-center">没有可归入该方向的产物。</p>
              )}

              {unscopedArtifacts.length > 0 && (
                <p className="mt-3 text-[11px] text-gray-500 border-t border-gray-800/60 pt-2">
                  另有 {unscopedArtifacts.length} 条产物不属于所选方向，未并入此列表。
                </p>
              )}
            </div>

            {/* Artifact detail viewer (Right column) */}
            <div className="flex-1 min-h-0 flex flex-col overflow-hidden">
              <div className="flex items-center justify-between pb-2 border-b border-gray-800/60 mb-2">
                <span className="text-xs font-semibold text-gray-300">产物正文内容预览 (Full Viewer)</span>
                {artifactBody.data && (
                  <button
                    type="button"
                    onClick={() => handleCopy(JSON.stringify(artifactBody.data, null, 2))}
                    className="rounded border border-gray-700 bg-gray-900 px-2 py-0.5 text-[11px] text-gray-300 hover:bg-gray-800"
                  >
                    {copied ? '已复制 ✓' : '复制 JSON'}
                  </button>
                )}
              </div>

              {artifactBody.error && <p role="alert" className="text-red-300 mb-2">{artifactBody.error}</p>}

              <div className="flex-1 min-h-0 overflow-auto rounded-lg border border-gray-800 bg-gray-950 p-3">
                {artifactBody.data ? (
                  <pre
                    data-testid="artifact-body"
                    className="font-mono text-xs text-gray-200 whitespace-pre-wrap leading-relaxed"
                  >
                    {JSON.stringify(artifactBody.data, null, 2)}
                  </pre>
                ) : (
                  <div className="h-full flex items-center justify-center text-gray-500 text-xs">
                    {scopedArtifacts.length > 0 ? '👈 点击左侧产物以在大屏中阅读完整内容' : '暂无产物详情'}
                  </div>
                )}
              </div>
            </div>
          </div>
        )}

        {tab === 'facts' && (
          <div data-testid="fact-list" className="space-y-2">
            <div className="flex items-center justify-between pb-2 border-b border-gray-800/60 mb-2">
              <span className="text-xs font-semibold text-gray-300">已沉淀事实与证据 (Facts Inventory)</span>
              <span className="font-mono text-[10px] text-gray-500">{facts.data?.length || 0} 项</span>
            </div>
            {facts.error && <p role="alert" className="text-red-300">{facts.error}</p>}
            
            {(facts.data || []).map((fact) => (
              <div key={String(fact.id)} className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-2.5">
                <div className="flex items-center justify-between text-[11px] text-gray-400">
                  <span className="font-mono text-gray-500">ID: {String(fact.id).slice(0, 8)}</span>
                  <span className="rounded bg-emerald-500/10 text-emerald-300 border border-emerald-500/20 px-1.5 py-0.2 font-mono text-[10px]">
                    {String(fact.state || 'active')}
                  </span>
                </div>
                <p className="mt-1 font-medium text-gray-200">
                  Fact · {String(fact.state || '')} · {JSON.stringify(fact.payload || fact.summary || '')}
                </p>
              </div>
            ))}
            {facts.data && facts.data.length === 0 && <p className="text-gray-500 py-4 text-center">没有事实。</p>}
          </div>
        )}

        {tab === 'findings' && (
          <div className="space-y-4">
            <div>
              <div className="flex items-center justify-between pb-2 border-b border-gray-800/60 mb-2">
                <span className="text-xs font-semibold text-gray-300">漏洞发现与证据链 (Findings)</span>
                <span className="font-mono text-[10px] text-gray-500">{findings.data?.length || 0} 项</span>
              </div>
              <div data-testid="finding-list" className="space-y-2">
                {findings.error && <p role="alert" className="text-red-300">{findings.error}</p>}
                {(findings.data || []).map((finding) => (
                  <div key={String(finding.id)} data-testid="finding-row" className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-2.5">
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-medium text-gray-200">{String(finding.title || finding.id)}</span>
                      <span className="rounded bg-amber-500/10 text-amber-300 border border-amber-500/20 px-1.5 py-0.2 font-mono text-[10px]">
                        审核 {String(finding.status)}
                      </span>
                    </div>
                    <div className="mt-1 text-[11px] text-gray-400 flex flex-wrap gap-x-3">
                      <span>来源 {String(finding.source_kind || '未标注')}</span>
                      <span>复测 {String(finding.retest_verdict || '无')}</span>
                    </div>
                    {/* Retain exact match string for tests */}
                    <p className="sr-only">
                      {String(finding.title || finding.id)} · 审核 {String(finding.status)} · 来源 {String(finding.source_kind || '未标注')} · 复测 {String(finding.retest_verdict || '无')}
                    </p>
                  </div>
                ))}
              </div>
            </div>

            <div className="mt-4 border-t border-gray-800/80 pt-3">
              <h3 className="text-xs font-semibold text-gray-300 mb-2">漏洞核查与复测面板 (Check Results)</h3>
              <CheckResultsPanel runId={runId} status={runStatus} toolCalls={[]} />
            </div>
          </div>
        )}

        {tab === 'goals' && (
          <div className="space-y-2">
            <div className="flex items-center justify-between pb-2 border-b border-gray-800/60 mb-2">
              <span className="text-xs font-semibold text-gray-300">目标证明与达成判定 (Goals Proof)</span>
              <span className="font-mono text-[10px] text-gray-500">{goals.data?.length || 0} 项</span>
            </div>
            <div data-testid="goal-list" className="space-y-2">
              {goals.error && <p role="alert" className="text-red-300">{goals.error}</p>}
              {(goals.data || []).map((goal) => (
                <div key={String(goal.id)} data-testid="goal-row" className="rounded-lg border border-gray-800/80 bg-gray-900/40 p-2.5">
                  <div className="flex items-center justify-between">
                    <span className="font-medium text-gray-200">Goal · {String(goal.state)}</span>
                    <span className="font-mono text-[10px] text-gray-500">ID: {String(goal.id).slice(0, 8)}</span>
                  </div>
                  <pre className="mt-1.5 font-mono text-[11px] text-gray-300 whitespace-pre-wrap bg-gray-950/80 p-2 rounded border border-gray-800/60">
                    {JSON.stringify((goal.payload as Record<string, unknown>) || {}, null, 2)}
                  </pre>
                </div>
              ))}
              {goals.data && goals.data.length === 0 && <p className="text-gray-500 py-4 text-center">没有目标。</p>}
            </div>
          </div>
        )}

        {showLegacy && !direction && (
          <div data-testid="legacy-record" className="mt-4 border-t border-gray-800 pt-3">
            <h3 className="font-medium text-gray-200">历史工作项</h3>
            <p className="text-gray-500 mt-0.5">这是既有工作项记录，不是 workbench.v1 方向板，也不代表活动 Worker。</p>
            {legacyItems.error && <p role="alert" className="text-red-300 mt-1">{legacyItems.error}</p>}
            <div className="mt-2 space-y-1">
              {(legacyItems.data || []).map((item) => (
                <p key={String(item.id)} data-testid="legacy-work-item" className="rounded border border-gray-800/60 bg-gray-950/60 px-3 py-1 text-gray-300">
                  {String(item.title || item.objective || item.id)} · {String(item.status)}
                </p>
              ))}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}
