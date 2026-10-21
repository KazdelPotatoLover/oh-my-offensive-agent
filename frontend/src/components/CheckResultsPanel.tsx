'use client';

import React, { useEffect, useState, useCallback } from 'react';
import { api, newRequestID } from '@/lib/api';
import { AgentToolCall, CheckResult, Finding, FindingReview, FindingReport, FindingRetest } from '@/lib/types';

const checkStatusLabels: Record<string, { label: string; color: string }> = {
  pass: { label: '检查通过', color: 'bg-emerald-500/10 text-emerald-400 border-emerald-500/30' },
  needs_attention: { label: '需要关注', color: 'bg-amber-500/10 text-amber-400 border-amber-500/30' },
  not_applicable: { label: '不适用', color: 'bg-gray-800 text-gray-400 border-gray-700' },
  insufficient_evidence: { label: '证据不足', color: 'bg-orange-500/10 text-orange-400 border-orange-500/30' },
  error: { label: '执行错误', color: 'bg-red-500/10 text-red-400 border-red-500/30' },
};

const findingStatusLabels: Record<string, { label: string; color: string; desc: string }> = {
  pending_review: {
    label: '待人工审核',
    color: 'bg-amber-500/20 text-amber-300 border-amber-500/40',
    desc: '程序规则判定的配置风险，等待安全人员确认或处置',
  },
  confirmed: {
    label: '人工已确认',
    color: 'bg-blue-500/20 text-blue-300 border-blue-500/40',
    desc: '人工确认当前发现及其证据；可利用性以具体证据为准',
  },
  false_positive: {
    label: '判定误报',
    color: 'bg-rose-500/20 text-rose-300 border-rose-500/40',
    desc: '目标场景或证据不构成风险，已标记为误报',
  },
  ignored: {
    label: '暂忽略',
    color: 'bg-slate-700/50 text-slate-300 border-slate-600',
    desc: '已知悉风险但暂不处理（注意：忽略不等于误报）',
  },
};

interface ReviewFormState {
  findingId: string;
  targetStatus: 'pending_review' | 'confirmed' | 'false_positive' | 'ignored';
  reviewer: string;
  comment: string;
  expectedVersion: number;
  requestId: string;
  submitting: boolean;
  error: string | null;
  conflict: boolean;
}

export function CheckResultsPanel({ runId, status, toolCalls }: {
  runId: string; status: string; toolCalls: AgentToolCall[];
}) {
  const [checks, setChecks] = useState<CheckResult[]>([]);
  const [findings, setFindings] = useState<Finding[]>([]);
  const [report, setReport] = useState<FindingReport | null>(null);
  const [reviewsMap, setReviewsMap] = useState<Record<string, FindingReview[]>>({});
  const [expandedHistory, setExpandedHistory] = useState<Record<string, boolean>>({});
  const [retestsMap, setRetestsMap] = useState<Record<string, FindingRetest[]>>({});
  const [expandedRetests, setExpandedRetests] = useState<Record<string, boolean>>({});
  const [retestForm, setRetestForm] = useState<{ findingId: string; notes: string; submitting: boolean; error: string | null } | null>(null);
  const [finishRetestForm, setFinishRetestForm] = useState<{ findingId: string; retestId: string; verdict: 'still_reproducible' | 'fixed' | 'inconclusive'; evidence: string; summary: string; submitting: boolean; error: string | null } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [reviewForm, setReviewForm] = useState<ReviewFormState | null>(null);

  const loadRetestsForFinding = async (findingId: string) => {
    try {
      const rts = await api.listFindingRetests(runId, findingId);
      setRetestsMap(prev => ({ ...prev, [findingId]: rts }));
    } catch (err) {
      console.error('Failed to load retests for finding', findingId, err);
    }
  };

  const toggleRetests = async (findingId: string) => {
    const isExpanded = !!expandedRetests[findingId];
    if (!isExpanded && !retestsMap[findingId]) {
      await loadRetestsForFinding(findingId);
    }
    setExpandedRetests(prev => ({ ...prev, [findingId]: !isExpanded }));
  };

  const submitCreateRetest = async () => {
    if (!retestForm) return;
    setRetestForm(prev => prev ? { ...prev, submitting: true, error: null } : null);
    try {
      await api.createFindingRetest(runId, retestForm.findingId, retestForm.notes);
      await loadData();
      await loadRetestsForFinding(retestForm.findingId);
      setExpandedRetests(prev => ({ ...prev, [retestForm.findingId]: true }));
      setRetestForm(null);
    } catch (err: any) {
      setRetestForm(prev => prev ? { ...prev, submitting: false, error: err.message || '发起复测失败' } : null);
    }
  };

  const submitFinishRetest = async () => {
    if (!finishRetestForm) return;
    setFinishRetestForm(prev => prev ? { ...prev, submitting: true, error: null } : null);
    try {
      if (finishRetestForm.verdict !== 'inconclusive') {
        let reference: { source_tool_call_id?: unknown } | null = null;
        try { reference = JSON.parse(finishRetestForm.evidence); } catch { /* Report the actionable validation message below. */ }
        if (!reference || typeof reference.source_tool_call_id !== 'string' || !reference.source_tool_call_id.trim()) {
          throw new Error('请引用本次复测的目标验证记录；证据不足时请选择“无法确认”。');
        }
      }
      await api.finishFindingRetest(runId, finishRetestForm.findingId, finishRetestForm.retestId, finishRetestForm.verdict, finishRetestForm.evidence, finishRetestForm.summary);
      await loadData();
      await loadRetestsForFinding(finishRetestForm.findingId);
      setFinishRetestForm(null);
    } catch (err: any) {
      setFinishRetestForm(prev => prev ? { ...prev, submitting: false, error: err.message || '提交复测结论失败' } : null);
    }
  };

  const loadData = useCallback(async () => {
    try {
      const [results, candidates, findingReport] = await Promise.all([
        api.getRunChecks(runId),
        api.getRunFindings(runId),
        api.getFindingReport(runId).catch(() => null),
      ]);
      setChecks(results);
      setFindings(candidates);
      setReport(findingReport);
      setError(null);
    } catch {
      setError('检查结果与报告读取失败，请确认后端服务状态。');
    } finally {
      setLoading(false);
    }
  }, [runId]);

  useEffect(() => {
    void loadData();
    const isActive = ['queued', 'running', 'stopping'].includes(status);
    if (isActive) {
      const timer = setInterval(loadData, 2000);
      return () => clearInterval(timer);
    }
  }, [runId, status, loadData]);

  useEffect(() => {
    const activeIds = Object.entries(retestsMap).filter(([, items]) => items.some(item => item.status === 'running')).map(([id]) => id);
    if (!activeIds.length) return;
    let polling = false;
    const timer = setInterval(async () => {
      if (polling) return;
      polling = true;
      try { await Promise.all([loadData(), ...activeIds.map(id => loadRetestsForFinding(id))]); }
      finally { polling = false; }
    }, 1000);
    return () => clearInterval(timer);
  }, [retestsMap, loadData]);

  const loadReviewsForFinding = async (findingId: string) => {
    try {
      const revs = await api.getFindingReviews(runId, findingId);
      setReviewsMap(prev => ({ ...prev, [findingId]: revs }));
    } catch (err) {
      console.error('Failed to load reviews for finding', findingId, err);
    }
  };

  const toggleHistory = async (findingId: string) => {
    const isExpanded = !!expandedHistory[findingId];
    if (!isExpanded && !reviewsMap[findingId]) {
      await loadReviewsForFinding(findingId);
    }
    setExpandedHistory(prev => ({ ...prev, [findingId]: !isExpanded }));
  };

  const openReviewForm = (finding: Finding) => {
    let savedReviewer = 'Auditor';
    if (typeof window !== 'undefined') {
      savedReviewer = localStorage.getItem('finding_reviewer_name') || 'Auditor';
    }
    let defaultNextStatus: 'pending_review' | 'confirmed' | 'false_positive' | 'ignored' = 'confirmed';
    if (finding.status === 'confirmed') {
      defaultNextStatus = 'false_positive';
    } else if (finding.status === 'false_positive' || finding.status === 'ignored') {
      defaultNextStatus = 'pending_review';
    }

    setReviewForm({
      findingId: finding.id,
      targetStatus: defaultNextStatus,
      reviewer: savedReviewer,
      comment: '',
      expectedVersion: finding.review_version,
      requestId: newRequestID(),
      submitting: false,
      error: null,
      conflict: false,
    });
  };

  const handleReviewSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!reviewForm) return;

    if (!reviewForm.reviewer.trim()) {
      setReviewForm(prev => prev ? { ...prev, error: '请填写审核人姓名（自报名称）' } : null);
      return;
    }
    if (!reviewForm.comment.trim()) {
      setReviewForm(prev => prev ? { ...prev, error: '请填写审核说明/理由（必填）' } : null);
      return;
    }

    setReviewForm(prev => prev ? { ...prev, submitting: true, error: null, conflict: false } : null);

    try {
      if (typeof window !== 'undefined') {
        localStorage.setItem('finding_reviewer_name', reviewForm.reviewer.trim());
      }

      await api.reviewFinding(runId, reviewForm.findingId, {
        status: reviewForm.targetStatus,
        reviewer: reviewForm.reviewer.trim(),
        comment: reviewForm.comment.trim(),
        expected_version: reviewForm.expectedVersion,
        request_id: reviewForm.requestId,
      });

      // Reload findings, report, and reviews for this finding
      await loadData();
      await loadReviewsForFinding(reviewForm.findingId);
      setReviewForm(null);
    } catch (err: any) {
      const errMsg = err?.message || '审核提交失败';
      const isConflict = errMsg.includes('version mismatch') || errMsg.includes('same status') || errMsg.includes('conflict');
      if (isConflict) {
        // Refresh data to show latest version to user
        await loadData();
        await loadReviewsForFinding(reviewForm.findingId);
      }
      setReviewForm(prev => prev ? {
        ...prev,
        submitting: false,
        error: isConflict ? '检测到数据或版本冲突（已有更新或相同状态变更），已为您同步最新数据，请重新核实后提交。' : errMsg,
        conflict: isConflict,
      } : null);
    }
  };

  return (
    <section className="bg-gray-900 border border-gray-800 rounded-2xl p-6 space-y-6 shadow-sm">
      {/* Header */}
      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-3 border-b border-gray-800 pb-4">
        <div>
          <h2 className="text-lg font-bold text-white flex items-center gap-2">
            <span>配置安全审查与人工审核闭环</span>
            <span className="text-xs px-2 py-0.5 rounded-full bg-indigo-500/20 text-indigo-300 font-mono">
              Phase 9
            </span>
          </h2>
          <p className="text-xs text-gray-400 mt-1">
            检查 {checks.length} 项 · 候选发现 {findings.length} 条。规则候选、实证来源、人工审核与复测结论分别保留。
          </p>
        </div>
        <button
          type="button"
          onClick={() => void loadData()}
          className="self-start sm:self-auto px-3 py-1.5 text-xs bg-gray-800 hover:bg-gray-700 text-gray-300 rounded-lg border border-gray-700 transition-colors flex items-center gap-1.5"
        >
          <svg className="w-3.5 h-3.5 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15" />
          </svg>
          <span>刷新审核与报告</span>
        </button>
      </div>

      {error && <p role="alert" className="text-sm text-amber-300 bg-amber-500/10 p-3 rounded-lg border border-amber-500/30">{error}</p>}
      {loading && <p className="text-sm text-gray-400">正在读取检查与审核数据…</p>}

      {/* Current Review Report View */}
      {report && (
        <div className="bg-gray-950 border border-gray-800 rounded-xl p-4 space-y-3">
          <div className="flex items-center justify-between">
            <div className="flex items-center gap-2">
              <span className="w-2 h-2 rounded-full bg-blue-400"></span>
              <h3 className="text-xs font-bold uppercase tracking-wider text-gray-300">
                当前审核报告 (Current Finding Review Report)
              </h3>
            </div>
            <span className="text-[11px] font-mono text-gray-500">
              快照时间: {new Date(report.generated_at).toLocaleTimeString()}
            </span>
          </div>

          <div className="grid grid-cols-2 sm:grid-cols-4 gap-3">
            <div className="p-3 rounded-lg bg-gray-900 border border-amber-500/30">
              <div className="text-[11px] text-amber-400 font-medium">待审核 (Pending)</div>
              <div className="text-xl font-bold font-mono text-white mt-0.5">{report.summary.pending_review}</div>
              <div className="text-[10px] text-gray-500 mt-0.5">尚未作出人工判断</div>
            </div>

            <div className="p-3 rounded-lg bg-gray-900 border border-blue-500/30">
              <div className="text-[11px] text-blue-400 font-medium">已确认 (Confirmed)</div>
              <div className="text-xl font-bold font-mono text-white mt-0.5">{report.summary.confirmed}</div>
              <div className="text-[10px] text-gray-500 mt-0.5">人工确认发现与证据</div>
            </div>

            <div className="p-3 rounded-lg bg-gray-900 border border-rose-500/30">
              <div className="text-[11px] text-rose-400 font-medium">误报 (False Positive)</div>
              <div className="text-xl font-bold font-mono text-white mt-0.5">{report.summary.false_positive}</div>
              <div className="text-[10px] text-gray-500 mt-0.5">不适用或证据不成立</div>
            </div>

            <div className="p-3 rounded-lg bg-gray-900 border border-slate-700">
              <div className="text-[11px] text-slate-400 font-medium">忽略 (Ignored)</div>
              <div className="text-xl font-bold font-mono text-white mt-0.5">{report.summary.ignored}</div>
              <div className="text-[10px] text-gray-500 mt-0.5">暂不处理（非误报）</div>
            </div>
          </div>
          <p className="text-[11px] text-gray-500 pt-1">
            * 报告直接从数据库生成当前快照；误报与忽略项单独归类。
          </p>
        </div>
      )}

      {!loading && !error && checks.length === 0 && (
        <p className="text-sm text-gray-400">尚无检查结果；资产已访问不代表已执行安全检查规则。</p>
      )}

      {/* Check Results & Associated Findings */}
      <div className="space-y-4">
        {checks.map(check => {
          const statusMeta = checkStatusLabels[check.status] || {
            label: check.status,
            color: 'bg-gray-800 text-gray-300 border-gray-700',
          };
          const associatedFindings = findings.filter(f => f.check_result_id === check.id);

          return (
            <details
              key={check.id}
              id={`check-${check.id}`}
              className="border border-gray-800 bg-gray-950/60 rounded-xl p-4 transition-all"
              open={associatedFindings.length > 0}
            >
              <summary className="cursor-pointer text-sm font-medium text-gray-200 flex items-center justify-between select-none">
                <div className="flex items-center gap-2">
                  <span className={`px-2 py-0.5 rounded text-xs font-mono border ${statusMeta.color}`}>
                    {statusMeta.label}
                  </span>
                  <span className="font-semibold text-white">{check.check_name}</span>
                  <span className="text-xs text-gray-500 font-mono">v{check.check_version}</span>
                </div>
                <span className="text-xs text-gray-500 font-mono">
                  {associatedFindings.length > 0 ? `${associatedFindings.length} 条候选发现` : '无发现'}
                </span>
              </summary>

              <div className="mt-4 space-y-4 text-sm text-gray-300 border-t border-gray-800/80 pt-3">
                <div>
                  <div className="text-xs text-gray-400 font-medium mb-1">规则判定解释:</div>
                  <p className="bg-gray-900/80 p-2.5 rounded-lg text-xs leading-relaxed text-gray-200 font-mono">
                    {check.explanation}
                  </p>
                </div>

                <div className="grid grid-cols-1 sm:grid-cols-2 gap-2 text-xs text-gray-400">
                  <div>资产 ID: <span className="font-mono text-gray-300">{check.asset_id}</span></div>
                  <div>结果 ID: <span className="font-mono text-gray-300">{check.id}</span></div>
                  <div className="sm:col-span-2">
                    来源证据工具调用: <span className="font-mono text-gray-300">{check.source_tool_call_id || '无持久化 HTTP 证据'}</span>
                  </div>
                </div>

                {/* Evidence Snapshot */}
                <div>
                  <div className="text-xs text-gray-400 font-medium mb-1">检查证据快照:</div>
                  <pre className="overflow-auto max-h-48 whitespace-pre-wrap text-[11px] font-mono bg-gray-950 p-3 rounded-lg border border-gray-800 text-gray-300">
                    {JSON.stringify(check.evidence_snapshot, null, 2)}
                  </pre>
                </div>

                {/* Candidate Findings list for this CheckResult */}
                {associatedFindings.map(finding => {
                  const fStatusMeta = findingStatusLabels[finding.status] || {
                    label: finding.status,
                    color: 'bg-gray-800 text-gray-300 border-gray-700',
                    desc: '',
                  };
                  const isAudited = finding.review_version > 0;
                  const isHistoryOpen = !!expandedHistory[finding.id];
                  const reviews = reviewsMap[finding.id] || [];

                  return (
                    <div
                      key={finding.id}
                      className="border border-indigo-500/30 bg-gray-900/90 rounded-xl p-4 space-y-3 mt-3 shadow-sm"
                    >
                      {/* Finding Title & Status Badge */}
                      <div className="flex flex-col sm:flex-row sm:items-center justify-between gap-2 border-b border-gray-800 pb-2.5">
                        <div className="space-y-1">
                          <div className="flex flex-wrap items-center gap-1.5">
                            <span className={`px-2 py-0.5 rounded text-xs font-semibold border ${fStatusMeta.color}`}>
                              {fStatusMeta.label}
                            </span>
                            <span className="text-xs font-mono uppercase px-1.5 py-0.5 rounded bg-gray-800 text-gray-300 border border-gray-700">
                              {finding.severity}
                            </span>
                            <span className="text-xs text-gray-400 font-mono">
                              v{finding.review_version}
                            </span>
                            {finding.source_kind === 'empirical' && (
                              <span className="px-1.5 py-0.5 rounded text-[11px] font-semibold bg-purple-500/20 text-purple-300 border border-purple-500/40">
                                实证发现
                              </span>
                            )}
                            {finding.source_kind === 'speculative' && (
                              <span className="px-1.5 py-0.5 rounded text-[11px] font-semibold bg-amber-500/20 text-amber-300 border border-amber-500/40">
                                推测线索
                              </span>
                            )}
                            {finding.verification_state === 'retested' ? (
                              <span className={`px-1.5 py-0.5 rounded text-[11px] font-semibold border ${
                                finding.retest_verdict === 'fixed'
                                  ? 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40'
                                  : 'bg-rose-500/20 text-rose-300 border-rose-500/40'
                              }`}>
                                复测: {finding.retest_verdict === 'fixed' ? '已修复' : finding.retest_verdict || '已复测'}
                              </span>
                            ) : (
                              <span className="px-1.5 py-0.5 rounded text-[11px] font-mono bg-gray-800 text-gray-400 border border-gray-700">
                                {finding.verification_state === 'verified' ? '来源已核验' : '未复测'}
                              </span>
                            )}
                          </div>
                          <h4 className="text-sm font-bold text-white pt-0.5">{finding.title}</h4>
                        </div>

                        {/* Action Buttons */}
                        <div className="flex items-center gap-2">
                          <button
                            type="button"
                            onClick={() => setRetestForm({ findingId: finding.id, notes: '', submitting: false, error: null })}
                            className="px-2.5 py-1 bg-purple-700 hover:bg-purple-600 text-white rounded text-xs font-medium transition-colors shadow-sm"
                          >
                            发起独立复测
                          </button>
                          <button
                            type="button"
                            onClick={() => openReviewForm(finding)}
                            className="px-3 py-1 bg-indigo-600 hover:bg-indigo-500 text-white rounded text-xs font-medium transition-colors shadow-sm"
                          >
                            {isAudited ? '修改审核结论' : '进行人工审核'}
                          </button>
                        </div>
                      </div>

                      {/* Description & Remediation */}
                      <div className="space-y-1.5 text-xs text-gray-300">
                        <p><strong className="text-gray-400">发现描述：</strong>{finding.description}</p>
                        {finding.remediation && (
                          <p><strong className="text-emerald-400">建议处置：</strong>{finding.remediation}</p>
                        )}
                        <p className="text-[11px] font-mono text-gray-500">
                          发现 ID: {finding.id} · 创建于: {new Date(finding.created_at).toLocaleString()}
                        </p>
                      </div>

                      {/* Latest Review Summary (if reviewed) */}
                      {isAudited && (
                        <div className="p-3 bg-gray-950/80 rounded-lg border border-gray-800 space-y-1 text-xs">
                          <div className="flex items-center justify-between text-gray-400">
                            <span className="font-semibold text-gray-200">
                              最后审核人: <span className="text-indigo-300">{finding.last_reviewer || '未知'}</span> (自报名称)
                            </span>
                            <span className="text-[11px] font-mono">
                              {finding.last_reviewed_at ? new Date(finding.last_reviewed_at).toLocaleString() : '-'}
                            </span>
                          </div>
                          <p className="text-gray-300 pt-0.5">
                            <span className="text-gray-500 font-medium">审核理由：</span>
                            {finding.last_review_comment || '无审核备注'}
                          </p>
                        </div>
                      )}

                      {/* Review Form (Inline Modal / Drawer) */}
                      {reviewForm && reviewForm.findingId === finding.id && (
                        <form
                          onSubmit={handleReviewSubmit}
                          className="p-4 bg-gray-950 rounded-xl border border-indigo-500/50 space-y-3 text-xs"
                        >
                          <div className="flex items-center justify-between border-b border-gray-800 pb-2">
                            <h5 className="font-bold text-white text-sm">
                              {isAudited ? '修正审核结论 / 重新打开' : '提交人工审核结论'}
                            </h5>
                            <button
                              type="button"
                              onClick={() => setReviewForm(null)}
                              className="text-gray-400 hover:text-white"
                            >
                              ✕
                            </button>
                          </div>

                          {/* Explanatory Callout */}
                          <div className="p-2.5 rounded bg-blue-500/10 border border-blue-500/30 text-[11px] text-blue-300 space-y-1">
                            <p>请核对本条发现的目标、原始响应与复现条件。人工确认保留为审核结论；来源校验和独立复测分别记录。</p>
                            <p>💡 <strong>忽略</strong>表示出于业务考量暂缓处理，<strong>不等于误报</strong>。</p>
                          </div>

                          {reviewForm.error && (
                            <div className="p-2.5 rounded bg-red-500/10 border border-red-500/30 text-red-300 text-[11px]">
                              {reviewForm.error}
                            </div>
                          )}

                          {/* Status Selection */}
                          <div>
                            <label className="block text-gray-300 font-medium mb-1.5">审核结论 (必选):</label>
                            <div className="grid grid-cols-2 sm:grid-cols-4 gap-2">
                              {(['confirmed', 'false_positive', 'ignored', 'pending_review'] as const).map(st => {
                                const isSelected = reviewForm.targetStatus === st;
                                const meta = findingStatusLabels[st];
                                return (
                                  <button
                                    key={st}
                                    type="button"
                                    onClick={() => setReviewForm(prev => prev ? {
                                      ...prev,
                                      targetStatus: st,
                                      expectedVersion: finding.review_version, // sync with latest finding version
                                    } : null)}
                                    className={`p-2 rounded-lg border text-left transition-colors ${
                                      isSelected
                                        ? 'border-indigo-500 bg-indigo-500/20 text-white font-semibold'
                                        : 'border-gray-800 bg-gray-900 text-gray-300 hover:border-gray-700'
                                    }`}
                                  >
                                    <div className="text-xs">{meta.label}</div>
                                  </button>
                                );
                              })}
                            </div>
                          </div>

                          {/* Reviewer Field */}
                          <div>
                            <label className="block text-gray-300 font-medium mb-1">
                              审核人自报姓名 (必填):
                              <span className="text-[10px] text-gray-500 font-normal ml-2">非身份认证体系，仅作审计自报说明</span>
                            </label>
                            <input
                              type="text"
                              value={reviewForm.reviewer}
                              onChange={e => setReviewForm(prev => prev ? { ...prev, reviewer: e.target.value } : null)}
                              className="w-full px-3 py-1.5 bg-gray-900 border border-gray-700 rounded-lg text-white font-mono focus:outline-none focus:border-indigo-500"
                              placeholder="例如：Alice"
                              maxLength={100}
                              disabled={reviewForm.submitting}
                              required
                            />
                          </div>

                          {/* Comment Field */}
                          <div>
                            <label className="block text-gray-300 font-medium mb-1">
                              审核理由与说明 (必填):
                            </label>
                            <textarea
                              value={reviewForm.comment}
                              onChange={e => setReviewForm(prev => prev ? { ...prev, comment: e.target.value } : null)}
                              className="w-full px-3 py-1.5 bg-gray-900 border border-gray-700 rounded-lg text-white text-xs focus:outline-none focus:border-indigo-500 h-20"
                              placeholder="请详述依据：例如 HTTP 响应头未包含 X-Content-Type-Options: nosniff，确认存在配置缺陷..."
                              maxLength={2000}
                              disabled={reviewForm.submitting}
                              required
                            />
                          </div>

                          {/* Buttons */}
                          <div className="flex items-center justify-end gap-2 pt-2 border-t border-gray-800">
                            <button
                              type="button"
                              onClick={() => setReviewForm(null)}
                              disabled={reviewForm.submitting}
                              className="px-3 py-1.5 rounded bg-gray-800 hover:bg-gray-700 text-gray-300 transition-colors"
                            >
                              取消
                            </button>
                            <button
                              type="submit"
                              disabled={reviewForm.submitting}
                              className="px-4 py-1.5 rounded bg-indigo-600 hover:bg-indigo-500 text-white font-medium transition-colors shadow disabled:opacity-50 flex items-center gap-1.5"
                            >
                              {reviewForm.submitting ? '提交中...' : '确认并更新审核结论'}
                            </button>
                          </div>
                        </form>
                      )}

                      {/* Review History Toggle & List */}
                      <div className="pt-1 border-t border-gray-800/60">
                        <button
                          type="button"
                          onClick={() => void toggleHistory(finding.id)}
                          className="text-xs text-indigo-400 hover:text-indigo-300 flex items-center gap-1 font-medium"
                        >
                          <svg
                            className={`w-3.5 h-3.5 transform transition-transform ${isHistoryOpen ? 'rotate-90' : ''}`}
                            fill="none"
                            viewBox="0 0 24 24"
                            stroke="currentColor"
                          >
                            <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M9 5l7 7-7 7" />
                          </svg>
                          <span>审核历史审计记录 {reviews.length > 0 ? `(${reviews.length})` : ''}</span>
                        </button>

                        {isHistoryOpen && (
                          <div className="mt-2 space-y-2 pl-4 border-l-2 border-gray-800">
                            {reviews.length === 0 ? (
                              <p className="text-xs text-gray-500 italic">尚无历史审核转换记录（初始待审核）。</p>
                            ) : (
                              reviews.map(rev => (
                                <div key={rev.id} className="text-xs p-2.5 rounded bg-gray-950 border border-gray-800 space-y-1 font-sans">
                                  <div className="flex items-center justify-between text-gray-400">
                                    <span className="font-semibold text-gray-200">
                                      版本: v{rev.to_version} ({rev.from_status} ➔ {rev.to_status})
                                    </span>
                                    <span className="text-[11px] font-mono">
                                      {new Date(rev.created_at).toLocaleString()}
                                    </span>
                                  </div>
                                  <div className="text-gray-300">
                                    <span className="text-gray-500">审核人: </span>{rev.reviewer} · <span className="text-gray-500">理由: </span>{rev.comment}
                                  </div>
                                  <div className="text-[10px] text-gray-600 font-mono">
                                    Request ID: {rev.request_id}
                                  </div>
                                </div>
                              ))
                            )}
                          </div>
                        )}
                      </div>

                      {/* Retest Form */}
                      {retestForm && retestForm.findingId === finding.id && (
                        <div className="mt-3 p-3.5 bg-gray-950 rounded-lg border border-purple-500/40 space-y-2.5">
                          <h5 className="text-xs font-bold text-purple-300">发起独立复测验证 (Verification Session)</h5>
                          <p className="text-[11px] text-gray-400">
                            将在独立会话中重放/复测本漏洞，不影响原扫描数据与历史审核记录。
                          </p>
                          <input
                            type="text"
                            placeholder="复测备注 (例如: 修复补丁已发布至测试环境)"
                            value={retestForm.notes}
                            onChange={e => setRetestForm({ ...retestForm, notes: e.target.value })}
                            className="w-full text-xs bg-gray-900 border border-gray-700 rounded px-3 py-1.5 text-white"
                          />
                          {retestForm.error && (
                            <p className="text-xs text-rose-400">{retestForm.error}</p>
                          )}
                          <div className="flex justify-end gap-2 pt-1">
                            <button
                              type="button"
                              onClick={() => setRetestForm(null)}
                              className="px-3 py-1 text-xs text-gray-400 hover:text-white"
                            >
                              取消
                            </button>
                            <button
                              type="button"
                              onClick={submitCreateRetest}
                              disabled={retestForm.submitting}
                              className="px-3.5 py-1 text-xs bg-purple-600 hover:bg-purple-500 text-white rounded font-medium disabled:opacity-50"
                            >
                              {retestForm.submitting ? '启动中...' : '确认启动复测'}
                            </button>
                          </div>
                        </div>
                      )}

                      {/* Finish Retest Form */}
                      {finishRetestForm && finishRetestForm.findingId === finding.id && (
                        <div className="mt-3 p-3.5 bg-gray-950 rounded-lg border border-emerald-500/40 space-y-2.5">
                          <h5 className="text-xs font-bold text-emerald-300">提交复测验证结论</h5>
                          <p className="text-[11px] text-gray-400">“已修复”和“仍可复现”需要本轮目标响应符合发现中保存的验证条件。普通说明文字不能作为验证结论。</p>
                          <div className="flex gap-2">
                            {(['still_reproducible', 'fixed', 'inconclusive'] as const).map(vd => (
                              <button
                                key={vd}
                                type="button"
                                onClick={() => setFinishRetestForm({ ...finishRetestForm, verdict: vd })}
                                className={`px-2.5 py-1 text-xs rounded border ${
                                  finishRetestForm.verdict === vd
                                    ? 'bg-indigo-600 text-white border-indigo-500 font-bold'
                                    : 'bg-gray-900 text-gray-400 border-gray-700'
                                }`}
                              >
                                {vd === 'fixed' ? '已修复 (Fixed)' : vd === 'still_reproducible' ? '仍可复现 (Reproducible)' : '无法确认 (Inconclusive)'}
                              </button>
                            ))}
                          </div>
                          <textarea
                            rows={2}
                            placeholder={finishRetestForm.verdict === 'inconclusive' ? '说明无法确认的原因...' : '{"source_tool_call_id":"本次复测的验证调用 UUID"}'}
                            value={finishRetestForm.evidence}
                            onChange={e => setFinishRetestForm({ ...finishRetestForm, evidence: e.target.value })}
                            className="w-full text-xs bg-gray-900 border border-gray-700 rounded p-2 text-white"
                          />
                          <input
                            type="text"
                            placeholder="复测总结说明..."
                            value={finishRetestForm.summary}
                            onChange={e => setFinishRetestForm({ ...finishRetestForm, summary: e.target.value })}
                            className="w-full text-xs bg-gray-900 border border-gray-700 rounded px-3 py-1.5 text-white"
                          />
                          {finishRetestForm.error && (
                            <p className="text-xs text-rose-400">{finishRetestForm.error}</p>
                          )}
                          <div className="flex justify-end gap-2 pt-1">
                            <button
                              type="button"
                              onClick={() => setFinishRetestForm(null)}
                              className="px-3 py-1 text-xs text-gray-400 hover:text-white"
                            >
                              取消
                            </button>
                            <button
                              type="button"
                              onClick={submitFinishRetest}
                              disabled={finishRetestForm.submitting}
                              className="px-3.5 py-1 text-xs bg-emerald-600 hover:bg-emerald-500 text-white rounded font-medium disabled:opacity-50"
                            >
                              {finishRetestForm.submitting ? '提交中...' : '提交复测结果'}
                            </button>
                          </div>
                        </div>
                      )}

                      {/* Retests History Toggle & List */}
                      <div className="pt-2 border-t border-gray-800/60">
                        <button
                          type="button"
                          onClick={() => void toggleRetests(finding.id)}
                          className="text-xs text-purple-400 hover:text-purple-300 flex items-center gap-1 font-medium"
                        >
                          <span>独立复测记录 {(retestsMap[finding.id] || []).length > 0 ? `(${(retestsMap[finding.id] || []).length})` : ''}</span>
                        </button>

                        {!!expandedRetests[finding.id] && (
                          <div className="mt-2 space-y-2 pl-4 border-l-2 border-purple-800">
                            {(retestsMap[finding.id] || []).length === 0 ? (
                              <p className="text-xs text-gray-500 italic">尚无独立复测记录。</p>
                            ) : (
                              (retestsMap[finding.id] || []).map(rt => (
                                <div key={rt.id} className="text-xs p-2.5 rounded bg-gray-950 border border-gray-800 space-y-1 font-sans">
                                  <div className="flex items-center justify-between">
                                    <span className="font-semibold text-gray-200 flex items-center gap-2">
                                      <span>状态: <span className="font-mono text-indigo-300">{rt.status}</span></span>
                                      {rt.verdict && (
                                        <span className={`px-1.5 py-0.2 rounded text-[10px] font-bold border ${
                                          rt.verdict === 'fixed' ? 'bg-emerald-500/20 text-emerald-300 border-emerald-500/40' : 'bg-red-500/20 text-red-300 border-red-500/40'
                                        }`}>
                                          结论: {rt.verdict}
                                        </span>
                                      )}
                                    </span>
                                    {rt.status === 'running' && (
                                      <button
                                        type="button"
                                        onClick={() => setFinishRetestForm({
                                          findingId: finding.id,
                                          retestId: rt.id,
                                          verdict: 'fixed',
                                          evidence: '',
                                          summary: '',
                                          submitting: false,
                                          error: null,
                                        })}
                                        className="px-2 py-0.5 bg-emerald-700 hover:bg-emerald-600 text-white rounded text-[11px]"
                                      >
                                        录入复测结论
                                      </button>
                                    )}
                                  </div>
                                  {rt.notes && <p className="text-gray-400">备注: {rt.notes}</p>}
                                  {rt.summary && <p className="text-gray-300">总结: {rt.summary}</p>}
                                  {rt.evidence && <p className="text-[11px] font-mono text-gray-400 bg-gray-900 p-1.5 rounded overflow-auto max-h-24">证据: {rt.evidence}</p>}
                                  <div className="text-[10px] text-gray-500">
                                    创建: {new Date(rt.created_at).toLocaleString()} {rt.finished_at ? `· 完成: ${new Date(rt.finished_at).toLocaleString()}` : ''}
                                  </div>
                                </div>
                              ))
                            )}
                          </div>
                        )}
                      </div>
                    </div>
                  );
                })}

                {/* Raw HTTP Evidence Disclosure */}
                {check.source_tool_call_id && (
                  <details className="pt-2 border-t border-gray-800/80">
                    <summary className="cursor-pointer text-xs text-indigo-400 hover:text-indigo-300">
                      查看原始 HTTP 工具证据 (get_endpoint)
                    </summary>
                    <pre className="mt-2 overflow-auto max-h-80 whitespace-pre-wrap text-[11px] font-mono bg-gray-950 p-3 rounded-lg border border-gray-800 text-gray-300">
                      {toolCalls.find(call => call.id === check.source_tool_call_id)?.output || '原始证据尚未在当前列表中加载，请在工具调用页查看对应工具编号。'}
                    </pre>
                  </details>
                )}
              </div>
            </details>
          );
        })}
      </div>
    </section>
  );
}
