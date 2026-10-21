'use client';

import React, { useCallback, useEffect, useState } from 'react';
import Link from 'next/link';
import { api } from '@/lib/api';

type Node = { id: string; kind: string; state: string; content_version: number; payload: Record<string, unknown>; work_item_id?: string; finding_id?: string };
type Artifact = { id: string; name: string; byte_length: number; sha256: string; work_item_id?: string; source_tool_call_id?: string };
type Edge = { id: string; src_id: string; dst_id: string; rel: string; valid?: boolean; invalid_reason?: string };

export function ExplorationResources({ runId, onChanged }: { runId: string; onChanged?: () => Promise<void> }) {
  const [open, setOpen] = useState(false);
  const [error, setError] = useState('');
  const [profiles, setProfiles] = useState<Record<string, unknown>[]>([]);
  const [artifacts, setArtifacts] = useState<Artifact[]>([]);
  const [artifactOffset, setArtifactOffset] = useState(0);
  const [artifactMore, setArtifactMore] = useState(false);
  const [checkpoints, setCheckpoints] = useState<Record<string, unknown>[]>([]);
  const [nodes, setNodes] = useState<Node[]>([]);
  const [nodeOffset, setNodeOffset] = useState(0);
  const [nodeMore, setNodeMore] = useState(false);
  const [detail, setDetail] = useState<{ node: Node; incoming_edges: Edge[]; outgoing_edges: Edge[]; anchors: string[] } | null>(null);
  const [output, setOutput] = useState<{ artifact_id: string; content: string; next_offset: number; total_bytes: number; has_more: boolean } | null>(null);
  const [reason, setReason] = useState('');
  const [goalText, setGoalText] = useState('');
  const [busy, setBusy] = useState(false);
  const [hint, setHint] = useState('');
  const [hintAssets, setHintAssets] = useState('');
  const [evidenceID, setEvidenceID] = useState('');
  const [proofReason, setProofReason] = useState('');

  const reload = useCallback(async () => {
    setError('');
    try {
      const [cap, art, cps, graph] = await Promise.all([
        api.getCapabilities(runId), api.getArtifacts(runId, artifactOffset), api.getCheckpoints(runId), api.getGraphPage(runId, nodeOffset),
      ]);
      setProfiles(cap.profiles); setArtifacts(art.artifacts); setArtifactMore(art.has_more);
      setCheckpoints(cps.checkpoints); setNodes(graph.nodes); setNodeMore(graph.has_more);
    } catch (e) { setError(e instanceof Error ? e.message : '读取探索资料失败'); }
  }, [runId, artifactOffset, nodeOffset]);
  useEffect(() => { if (open) void reload(); }, [open, reload]);

  const showNode = async (id: string) => {
    setError('');
    try { const data = await api.getNodeDetail(runId, id); setDetail(data); setReason(''); setGoalText(String(data.node.payload.text || '')); }
    catch (e) { setError(e instanceof Error ? e.message : '读取节点失败'); }
  };
  const showArtifact = async (id: string, offset = 0) => {
    setError('');
    try { setOutput(await api.readArtifact(runId, id, offset)); }
    catch (e) { setError(e instanceof Error ? e.message : '读取产物失败'); }
  };
  const revise = async () => {
    if (!detail) return;
    setBusy(true); setError('');
    try {
      await api.reviseNode(runId, detail.node.id, {
        expected_version: detail.node.content_version, actor: 'operator', reason,
        state: detail.node.kind === 'fact' ? 'retracted' : 'open',
        payload: detail.node.kind === 'goal' ? { ...detail.node.payload, text: goalText } : undefined,
      });
      await showNode(detail.node.id); await reload(); await onChanged?.();
    } catch (e) { setError(e instanceof Error ? e.message : '修改失败，请刷新后重试'); }
    finally { setBusy(false); }
  };
  const addHint = async () => {
    setBusy(true); setError('');
    try {
      await api.addHint(runId, hint, hintAssets.split(/[\s,]+/).filter(Boolean));
      setHint(''); setHintAssets(''); await reload(); await onChanged?.();
    } catch (e) { setError(e instanceof Error ? e.message : '提交线索失败'); }
    finally { setBusy(false); }
  };
  const prove = async () => {
    if (!detail) return;
    setBusy(true); setError('');
    try {
      await api.proveGoal(runId, detail.node.id, evidenceID.trim(), proofReason);
      await showNode(detail.node.id); await reload(); await onChanged?.();
    } catch (e) { setError(e instanceof Error ? e.message : '验证目标失败'); }
    finally { setBusy(false); }
  };
  const button = 'px-2 py-1 rounded border border-gray-700 text-xs text-indigo-300 hover:border-indigo-400 disabled:opacity-40';
  return (
    <section className="border border-gray-800 rounded-xl bg-gray-900/40" data-testid="exploration-resources">
      <button type="button" onClick={() => setOpen(!open)} className="w-full text-left p-4 text-sm font-semibold text-white">{open ? '▾' : '▸'} 能力、完整产物与探索关系</button>
      {open && <div className="p-4 border-t border-gray-800 space-y-6">
        <button type="button" onClick={() => { void reload(); void onChanged?.(); }} className={button}>刷新资料</button>
        {error && <p role="alert" className="text-sm text-rose-400">{error}</p>}
        <div className="space-y-2">
          <h3 className="text-sm text-indigo-300">给 Planner 的人工线索</h3>
          <textarea aria-label="人工线索" value={hint} onChange={e => setHint(e.target.value)} placeholder="说明观察或建议；不会更改原始目标或扩大授权范围。" className="w-full p-2 bg-gray-950 border border-gray-700 rounded text-xs text-gray-300" />
          <input aria-label="线索关联资产" value={hintAssets} onChange={e => setHintAssets(e.target.value)} placeholder="可选：本 Run 资产 UUID，以逗号分隔" className="w-full p-2 bg-gray-950 border border-gray-700 rounded text-xs text-gray-300" />
          <button className={button} disabled={busy || !hint.trim()} onClick={() => void addHint()}>提交人工线索</button>
        </div>
        <div>
          <h3 className="text-sm text-indigo-300 mb-2">本次执行的能力配置</h3>
          <div className="grid md:grid-cols-3 gap-2">{profiles.map(p => <div key={String(p.name)} className="p-3 bg-gray-950 rounded border border-gray-800 text-xs text-gray-300"><strong>{String(p.name)} · v{String(p.version)}</strong><p className="mt-1 break-words">{(p.tools as { name: string; disabled?: boolean }[]).filter(t => !t.disabled).map(t => t.name).join(' · ')}</p></div>)}</div>
        </div>
        <div>
          <h3 className="text-sm text-indigo-300 mb-2">完整产物</h3>
          {artifacts.length === 0 && <p className="text-xs text-gray-500">尚无保存的产物。</p>}
          <ul className="space-y-2">{artifacts.map(a => <li key={a.id} className="text-xs text-gray-400 p-2 bg-gray-950 rounded flex flex-wrap gap-3 items-center"><button className={button} onClick={() => void showArtifact(a.id)}>{a.name}</button><span>{a.byte_length.toLocaleString()} bytes</span><span className="font-mono" title={a.sha256}>sha256:{a.sha256.slice(0, 16)}</span>{a.source_tool_call_id && <span className="break-all">验证来源：{a.source_tool_call_id}</span>}</li>)}</ul>
          <div className="flex gap-2 mt-2"><button className={button} disabled={artifactOffset === 0} onClick={() => setArtifactOffset(Math.max(0, artifactOffset - 25))}>上一页产物</button><button className={button} disabled={!artifactMore} onClick={() => setArtifactOffset(artifactOffset + 25)}>下一页产物</button></div>
          {output && <div className="mt-3 p-3 border border-gray-700 rounded"><p className="text-xs text-gray-400 mb-2">已读至 {output.next_offset} / {output.total_bytes} bytes</p><pre className="text-xs text-gray-300 whitespace-pre-wrap break-all max-h-80 overflow-auto">{output.content}</pre><div className="flex gap-2 mt-2"><button className={button} onClick={() => void showArtifact(output.artifact_id)}>从头读取</button><button className={button} disabled={!output.has_more} onClick={() => void showArtifact(output.artifact_id, output.next_offset)}>继续读取</button></div></div>}
        </div>
        <div>
          <h3 className="text-sm text-indigo-300 mb-2">探索来源与证明</h3>
          <div className="flex flex-wrap gap-2">{nodes.map(n => <button key={n.id} className={button} onClick={() => void showNode(n.id)}>{n.kind} · {String(n.payload.text || n.payload.summary || n.payload.title || n.id).slice(0, 70)} · {n.state}</button>)}</div>
          <div className="flex gap-2 mt-2"><button className={button} disabled={nodeOffset === 0} onClick={() => setNodeOffset(Math.max(0, nodeOffset - 25))}>上一页节点</button><button className={button} disabled={!nodeMore} onClick={() => setNodeOffset(nodeOffset + 25)}>下一页节点</button></div>
          {detail && <div className="mt-3 p-3 bg-gray-950 rounded space-y-3 text-xs text-gray-300">
            <p>{detail.node.kind} · {detail.node.state} · 内容版本 {detail.node.content_version}</p>
            <pre className="whitespace-pre-wrap break-all">{JSON.stringify(detail.node.payload, null, 2)}</pre>
            {detail.node.work_item_id && <p>关联方向：{detail.node.work_item_id}（在编排视图查看 Attempt 和过程）</p>}
            {detail.anchors?.map(id => <Link className="text-indigo-300 underline block" key={id} href={`/assets/${id}`}>关联资产 {id}</Link>)}
            <ul>{[...(detail.incoming_edges || []), ...(detail.outgoing_edges || [])].map(e => <li key={e.id} className="flex flex-wrap gap-2 items-center mb-1"><button className={button} onClick={() => void showNode(e.src_id)}>来源节点</button><span>{e.rel}{e.valid === false ? ` · 已失效：${e.invalid_reason || '需要复核'}` : ''}</span><button className={button} onClick={() => void showNode(e.dst_id)}>目标节点</button></li>)}</ul>
            {detail.node.kind === 'goal' && detail.node.state !== 'abandoned' && <div className="space-y-2 border-t border-gray-800 pt-3">
              <input aria-label="证明证据 ID" value={evidenceID} onChange={e => setEvidenceID(e.target.value)} placeholder="已核验事实节点或发现的 UUID" className="w-full p-2 bg-gray-900 border border-gray-700 rounded" />
              <input aria-label="证明说明" value={proofReason} onChange={e => setProofReason(e.target.value)} placeholder="说明这份证据如何满足该目标" className="w-full p-2 bg-gray-900 border border-gray-700 rounded" />
              <button className={button} disabled={busy || !evidenceID.trim() || !proofReason.trim()} onClick={() => void prove()}>提交证据验证目标</button>
            </div>}
            {(detail.node.kind === 'goal' || detail.node.kind === 'fact') && <div className="border-t border-gray-800 pt-3 space-y-2">
              {detail.node.kind === 'goal' && <textarea aria-label="修改目标" value={goalText} onChange={e => setGoalText(e.target.value)} className="w-full p-2 bg-gray-900 border border-gray-700 rounded" />}
              <input aria-label="修订理由" placeholder="填写修改目标或撤回事实的理由" value={reason} onChange={e => setReason(e.target.value)} className="w-full p-2 bg-gray-900 border border-gray-700 rounded" />
              <p className="text-gray-500">保留原记录与证明历史；受影响的目标需要重新证明。</p>
              <button className={button} disabled={busy || !reason.trim()} onClick={() => void revise()}>{detail.node.kind === 'fact' ? '撤回事实' : '修订目标并重新验证'}</button>
            </div>}
          </div>}
        </div>
        <div>
          <h3 className="text-sm text-indigo-300 mb-2">上下文检查点</h3>
          {checkpoints.length === 0 ? <p className="text-xs text-gray-500">当前对话尚未触发压缩；原始过程持续保留。</p> : checkpoints.map(c => <details key={String(c.id)} className="text-xs p-2 border border-gray-800 rounded mb-2"><summary className="text-gray-300 cursor-pointer">覆盖 {String(c.covered_messages)} 条消息 · {String(c.strategy)}</summary><p className="text-gray-500 break-all mt-2">来源哈希：{String(c.source_sha256)}</p><pre className="text-gray-400 whitespace-pre-wrap mt-2">{String(c.summary)}</pre></details>)}
        </div>
      </div>}
    </section>
  );
}
