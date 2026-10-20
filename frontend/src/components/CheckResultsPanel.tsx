'use client';

import { useEffect, useState } from 'react';
import { api } from '@/lib/api';
import { AgentToolCall, CheckResult, Finding } from '@/lib/types';

const labels: Record<string, string> = {
  pass: '通过', needs_attention: '需要关注', not_applicable: '不适用',
  insufficient_evidence: '证据不足', error: '检查错误',
};

export function CheckResultsPanel({ runId, status, toolCalls }: {
  runId: string; status: string; toolCalls: AgentToolCall[];
}) {
  const [checks, setChecks] = useState<CheckResult[]>([]);
  const [findings, setFindings] = useState<Finding[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let disposed = false;
    let pending = false;
    async function load() {
      if (pending) return;
      pending = true;
      try {
        const [results, candidates] = await Promise.all([api.getRunChecks(runId), api.getRunFindings(runId)]);
        if (!disposed) { setChecks(results); setFindings(candidates); setError(null); }
      } catch {
        if (!disposed) setError('检查结果读取失败，请确认后端已更新后重试。');
      } finally {
        pending = false;
        if (!disposed) setLoading(false);
      }
    }
    void load();
    const timer = ['queued', 'running', 'stopping'].includes(status) ? setInterval(load, 2000) : undefined;
    return () => { disposed = true; if (timer) clearInterval(timer); };
  }, [runId, status]);

  return (
    <section className="bg-gray-900 border border-gray-800 rounded-2xl p-6 space-y-4">
      <h2 className="text-lg font-bold text-white">配置检查与候选发现</h2>
      <p className="text-sm text-gray-400">检查 {checks.length} 项 · 候选发现 {findings.length} 条。候选发现需要审核，不代表已验证的漏洞。</p>
      {error && <p role="alert" className="text-sm text-amber-300">{error}</p>}
      {loading && <p className="text-sm text-gray-400">正在读取检查结果…</p>}
      {!loading && !error && checks.length === 0 && <p className="text-sm text-gray-400">尚无检查结果；页面已访问不代表已完成安全检查。</p>}
      {checks.map(check => (
        <details key={check.id} id={`check-${check.id}`} className="border border-gray-800 rounded-xl p-4">
          <summary className="cursor-pointer text-sm text-gray-200">{labels[check.status] || check.status} · {check.check_name} · v{check.check_version}</summary>
          <div className="mt-3 space-y-2 text-sm text-gray-300">
            <p>{check.explanation}</p>
            <p className="break-all text-xs text-gray-400">资产：{check.asset_id}<br />检查结果：{check.id}<br />来源：{check.source_tool_call_id || '无可用 HTTP 证据'}</p>
            <pre className="overflow-auto max-h-64 whitespace-pre-wrap text-xs bg-gray-950 p-3 rounded">{JSON.stringify(check.evidence_snapshot, null, 2)}</pre>
            {findings.filter(f => f.check_result_id === check.id).map(finding => (
              <div key={finding.id} className="border border-amber-500/30 rounded-lg p-3 space-y-2">
                <p>{finding.title} · {finding.severity} · {finding.status === 'pending_review' ? '待审核' : finding.status}</p>
                <p>{finding.description}</p>
                <p>建议：{finding.remediation}</p>
                <p className="text-xs break-all">发现编号：{finding.id}</p>
              </div>
            ))}
            {check.source_tool_call_id && <details className="pt-2">
              <summary className="cursor-pointer">查看原始 HTTP 工具证据</summary>
              <pre className="mt-2 overflow-auto max-h-80 whitespace-pre-wrap text-xs bg-gray-950 p-3 rounded">{toolCalls.find(call => call.id === check.source_tool_call_id)?.output || '原始证据尚未加载，请在工具调用页查看对应来源编号。'}</pre>
            </details>}
          </div>
        </details>
      ))}
    </section>
  );
}
