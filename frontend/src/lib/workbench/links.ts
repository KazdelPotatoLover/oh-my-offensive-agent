export type WorkbenchSelection = { taskId?: string; runId?: string; workItemId?: string };

export function workbenchHref(selection: WorkbenchSelection): string {
  const { taskId, runId, workItemId } = selection;
  if (taskId && runId && workItemId) return `/workbench/tasks/${taskId}/runs/${runId}/work/${workItemId}`;
  if (taskId && runId) return `/workbench/tasks/${taskId}/runs/${runId}`;
  if (taskId) return `/workbench/tasks/${taskId}`;
  return '/workbench';
}

export function selectionFromPath(pathname: string, search = ''): WorkbenchSelection {
  const path = pathname.split('?')[0];
  const parts = path.split('/').filter(Boolean);
  const selection: WorkbenchSelection = {};
  const taskIndex = parts.indexOf('tasks');
  const runIndex = parts.indexOf('runs');
  const workIndex = parts.indexOf('work');
  if (taskIndex >= 0 && parts[taskIndex + 1]) selection.taskId = decodeURIComponent(parts[taskIndex + 1]);
  if (runIndex >= 0 && parts[runIndex + 1]) selection.runId = decodeURIComponent(parts[runIndex + 1]);
  if (workIndex >= 0 && parts[workIndex + 1]) selection.workItemId = decodeURIComponent(parts[workIndex + 1]);
  const params = new URLSearchParams(search.startsWith('?') ? search.slice(1) : search);
  if (!selection.taskId && params.get('task')) selection.taskId = params.get('task') || undefined;
  if (!selection.runId && params.get('run')) selection.runId = params.get('run') || undefined;
  if (!selection.workItemId && params.get('work')) selection.workItemId = params.get('work') || undefined;
  return selection;
}
