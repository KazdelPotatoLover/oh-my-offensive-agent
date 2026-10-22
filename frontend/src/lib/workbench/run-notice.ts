import type { Run } from '@/lib/types';

export function runNotice(run: Run | null): { title: string; detail: string } | null {
  if (!run || !['stopped', 'failed', 'interrupted'].includes(run.status)) return null;
  const reason = run.finish_reason || 'unknown';
  const summary = run.final_output?.split('\n\n')[0]?.trim() || '';
  if (reason === 'provider_error') {
    return { title: '模型服务调用失败，已有结果已保留', detail: '检查模型接口；服务恢复后可填写原因并继续任务。若预算不足，需先追加授权。' };
  }
  if (reason === 'needs_input') {
    return { title: '任务需要处理后继续，已有结果已保留', detail: summary || '检查预算和模型服务状态，再填写原因继续任务。' };
  }
  if (['model_calls_exhausted', 'tool_calls_exhausted', 'worker_budget_reserved'].includes(reason)) {
    return { title: '本轮可用额度已耗尽', detail: '剩余调用可能属于预留额度。请先追加所需授权，再填写原因继续任务；续跑不会清零历史用量。' };
  }
  return { title: `任务已结束：${reason}`, detail: summary || '已有结果可继续查看。' };
}
