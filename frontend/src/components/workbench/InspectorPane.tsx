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

  const allowed = new Set(direction?.allowed_actions || []);
  const steps = Array.isArray(trace.data?.steps) ? trace.data.steps as Record<string, unknown>[] : [];
  const scopedArtifacts = (artifacts.data || []).filter((item) => !workItemId || item.work_item_id === workItemId);
  const unscopedArtifacts = (artifacts.data || []).filter((item) => workItemId && item.work_item_id && item.work_item_id !== workItemId);

  return (
    <section aria-labelledby="inspector-heading" data-testid="inspector" className="flex h-full min-h-0 flex-col">
      <div className="border-b border-gray-800 px-3 py-2">
        <h2 id="inspector-heading" tabIndex={-1} className="text-sm font-semibold text-white">检查器</h2>
        {direction ? (
          <div className="mt-2 space-y-1 text-xs text-gray-300">
            <p data-testid="inspector-objective">{direction.objective || '未提供目标'}</p>
            <p>状态 <span data-testid="inspector-status">{direction.status}</span></p>
            <p>Attempt <span className="font-mono" data-testid="inspector-attempt">{direction.attempt_id || '无'}</span></p>
            <p>最新步骤 <span data-testid="inspector-step">{direction.latest_step || '未提供'}</span></p>
            <p>来源 {direction.source_refs?.length ? direction.source_refs.join(', ') : '未提供'}</p>
            <p className="font-mono break-all" data-testid="local-budget">局部额度 {direction.local_budget ? JSON.stringify(direction.local_budget) : '未提供'}</p>
            {direction.error && (
              <p role="alert" data-testid="direction-error">
                {direction.error.code} · {direction.error.scope} · {direction.error.message}
                {direction.error.suggested_action ? ` · ${direction.error.suggested_action}` : ''}
              </p>
            )}
          </div>
        ) : (
          <p className="mt-2 text-xs text-gray-400">{workItemId ? '所选方向不在当前快照中。' : '选择一个方向后查看过程。'}</p>
        )}
        <div className="mt-2 flex flex-wrap gap-1">
          <button type="button" disabled={!direction || !allowed.has('steer_work') || commandPending} onClick={() => onCommand('steer_work', { instruction })} className="rounded border border-gray-700 px-2 py-1 text-[11px] disabled:opacity-40">纠偏</button>
          <button type="button" data-testid="kill-direction" disabled={!direction || !allowed.has('kill_work') || commandPending} onClick={() => onCommand('kill_work', { reason: 'operator kill' })} className="rounded border border-red-800 px-2 py-1 text-[11px] text-red-200 disabled:opacity-40">终止该方向</button>
          <button type="button" disabled={!direction || !allowed.has('continue_work') || commandPending} onClick={() => onCommand('continue_work', instruction ? { instruction } : undefined)} className="rounded border border-gray-700 px-2 py-1 text-[11px] disabled:opacity-40">继续</button>
          <button type="button" disabled={!direction || !allowed.has('retry_work') || commandPending} onClick={() => onCommand('retry_work', instruction ? { instruction } : undefined)} className="rounded border border-gray-700 px-2 py-1 text-[11px] disabled:opacity-40">重试</button>
        </div>
        <label className="mt-2 block text-[11px] text-gray-400">方向指令
          <textarea aria-label="方向指令" value={instruction} onChange={(event) => setInstruction(event.target.value)} rows={2} className="mt-1 w-full rounded border border-gray-700 bg-gray-950 px-2 py-1 text-xs" />
        </label>
      </div>
      <div role="tablist" aria-label="检查器分区" className="flex gap-1 border-b border-gray-800 px-2 py-1">
        {([
          ['process', '过程'],
          ['artifacts', '产物'],
          ['facts', '事实'],
          ['findings', '发现'],
          ['goals', '目标'],
        ] as const).map(([id, label]) => (
          <button key={id} type="button" role="tab" aria-selected={tab === id} onClick={() => setTab(id)} className={`rounded px-2 py-1 text-[11px] ${tab === id ? 'bg-gray-800 text-white' : 'text-gray-400'}`}>{label}</button>
        ))}
      </div>
      <div className="min-h-0 flex-1 overflow-auto px-3 py-2 text-xs">
        {tab === 'process' && (
          <div data-testid="trace-panel">
            {trace.error && <p role="alert">{trace.error}</p>}
            {sessions.error && <p role="alert">{sessions.error}</p>}
            {steps.map((step) => (
              <p key={String(step.step_id)} data-testid="trace-step" className="border-b border-gray-800 py-1">
                {String(step.tool_name || 'unknown')} · {String(step.status || '')} · {String(step.summary || step.output || '')}
              </p>
            ))}
            {trace.data && steps.length === 0 && !trace.error && <p>这条 trace 没有步骤。</p>}
            <h3 className="mt-3 font-medium text-gray-200">历史 Attempt / Session</h3>
            {(sessions.data || []).map((session) => (
              <p key={String(session.id)} className="py-1 font-mono">{String(session.id)} · attempt {String(session.attempt_id || '无')} · {String(session.status)}</p>
            ))}
            {sessions.data && sessions.data.length === 0 && <p>没有该方向的 Session 记录。</p>}
          </div>
        )}
        {tab === 'artifacts' && (
          <div>
            {artifacts.error && <p role="alert">{artifacts.error}</p>}
            {scopedArtifacts.map((item) => (
              <button key={String(item.id)} type="button" data-testid="artifact-row" className="block w-full border-b border-gray-800 py-1 text-left" onClick={async () => {
                setArtifactBody(empty);
                try {
                  const body = asRecord(await readJson(`/api/runs/${runId}/artifacts/${item.id}?offset=0&limit=8192`));
                  setArtifactBody({ data: body || {}, error: null });
                } catch (err) {
                  setArtifactBody({ data: null, error: err instanceof Error ? err.message : '产物正文读取失败' });
                }
              }}>{String(item.name || item.id)}</button>
            ))}
            {artifacts.data && scopedArtifacts.length === 0 && <p>没有可归入该方向的产物。</p>}
            {unscopedArtifacts.length > 0 && <p className="mt-2 text-gray-500">另有 {unscopedArtifacts.length} 条产物不属于所选方向，未并入此列表。</p>}
            {artifactBody.error && <p role="alert">{artifactBody.error}</p>}
            {artifactBody.data && <pre data-testid="artifact-body" className="mt-2 whitespace-pre-wrap text-[11px] text-gray-300">{JSON.stringify(artifactBody.data, null, 2)}</pre>}
          </div>
        )}
        {tab === 'facts' && (
          <div data-testid="fact-list">
            {facts.error && <p role="alert">{facts.error}</p>}
            {(facts.data || []).map((fact) => <p key={String(fact.id)} className="border-b border-gray-800 py-1">Fact · {String(fact.state || '')} · {JSON.stringify(fact.payload || fact.summary || '')}</p>)}
            {facts.data && facts.data.length === 0 && <p>没有事实。</p>}
          </div>
        )}
        {tab === 'findings' && (
          <div>
            <div data-testid="finding-list">
              {findings.error && <p role="alert">{findings.error}</p>}
              {(findings.data || []).map((finding) => (
                <p key={String(finding.id)} data-testid="finding-row" className="border-b border-gray-800 py-1">
                  {String(finding.title || finding.id)} · 审核 {String(finding.status)} · 来源 {String(finding.source_kind || '未标注')} · 复测 {String(finding.retest_verdict || '无')}
                </p>
              ))}
            </div>
            <div className="mt-3">
              <CheckResultsPanel runId={runId} status={runStatus} toolCalls={[]} />
            </div>
          </div>
        )}
        {tab === 'goals' && (
          <div data-testid="goal-list">
            {goals.error && <p role="alert">{goals.error}</p>}
            {(goals.data || []).map((goal) => <p key={String(goal.id)} data-testid="goal-row" className="border-b border-gray-800 py-1">Goal · {String(goal.state)} · {JSON.stringify((goal.payload as Record<string, unknown>) || {})}</p>)}
            {goals.data && goals.data.length === 0 && <p>没有目标。</p>}
          </div>
        )}
        {showLegacy && !direction && (
          <div data-testid="legacy-record" className="mt-4 border-t border-gray-800 pt-2">
            <h3 className="font-medium text-gray-200">历史工作项</h3>
            <p className="text-gray-500">这是既有工作项记录，不是 workbench.v1 方向板，也不代表活动 Worker。</p>
            {legacyItems.error && <p role="alert">{legacyItems.error}</p>}
            {(legacyItems.data || []).map((item) => <p key={String(item.id)} data-testid="legacy-work-item">{String(item.title || item.objective || item.id)} · {String(item.status)}</p>)}
          </div>
        )}
      </div>
    </section>
  );
}
