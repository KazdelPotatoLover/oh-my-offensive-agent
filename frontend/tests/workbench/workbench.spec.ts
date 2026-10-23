import { expect, test, type Page } from '@playwright/test';
import { mkdir } from 'node:fs/promises';

const TASK = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const RUN = '11111111-1111-4111-8111-111111111111';
const LEGACY = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const TERMINAL = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';
const WORK = [
  '22222222-2222-4222-8222-222222222221',
  '22222222-2222-4222-8222-222222222222',
];
const FIXTURE = 'http://127.0.0.1:23196';

function runPath(runId = RUN, workId?: string) {
  const base = `/workbench/tasks/${TASK}/runs/${runId}`;
  return workId ? `${base}/work/${workId}` : base;
}

async function resetFixture() {
  const response = await fetch(`${FIXTURE}/__fixture/reset`, { method: 'POST' });
  if (!response.ok) throw new Error(`fixture reset ${response.status}`);
}

async function captionShot(page: Page, name: string) {
  await mkdir('evidence/workbench', { recursive: true });
  await page.evaluate(() => {
    const bar = document.createElement('div');
    bar.id = 'fixture-caption';
    bar.textContent = '合同替身证据，不是真实后端 N=4';
    bar.setAttribute('style', 'position:fixed;left:0;right:0;bottom:0;z-index:80;background:#7c2d12;color:#fff;font:12px/1.4 sans-serif;padding:4px 8px;');
    document.body.appendChild(bar);
  });
  await page.screenshot({ path: `evidence/workbench/${name}.png` });
  await page.evaluate(() => document.getElementById('fixture-caption')?.remove());
}

test.beforeEach(async () => {
  await resetFixture();
});

test('shows four directions and does not draw model calls as workers', async ({ page }) => {
  await page.goto(runPath());
  await expect(page.getByTestId('direction-card')).toHaveCount(4);
  await expect(page.getByTestId('worker-capacity')).toContainText('requested 4');
  await expect(page.getByTestId('worker-capacity')).toContainText('effective 4');
  await expect(page.getByTestId('worker-capacity')).toContainText('active 2');
  await expect(page.getByTestId('worker-count')).toContainText('方向 4');
  await expect(page.getByTestId('model-capacity')).toContainText('limit 2');
  await expect(page.getByTestId('model-call-count')).toContainText('6');
  await expect(page.getByTestId('origin-pacing')).toContainText('等待 是');
  await expect(page.getByTestId('run-epoch')).toContainText('Epoch 2');
  await expect(page.getByTestId('legacy-epoch')).toContainText('current_epoch 1');
  await expect(page.getByTestId('planner-status')).toContainText('waiting');
  await expect(page.getByTestId('execution-gaps')).toContainText('execution_input.agents');
  await expect(page.getByTestId('activity-stream')).toContainText('direction_progress');
  await captionShot(page, 'contract-fixture-n4');
});

test('selected direction shows attempt, tool and artifact from the API', async ({ page }) => {
  await page.goto(runPath(RUN, WORK[0]));
  await expect(page.getByTestId('inspector-objective')).toContainText('读取授权入口并保存响应');
  await expect(page.getByTestId('inspector-attempt')).toContainText('33333333-3333-4333-8333-333333333331');
  await expect(page.getByTestId('inspector-step')).toContainText('http_request');
  await expect(page.getByTestId('trace-step')).toContainText('http_request');
  await expect(page.getByTestId('local-budget')).toContainText('model_calls');
  await page.getByRole('tab', { name: '产物' }).click();
  await page.getByTestId('artifact-row').click();
  await expect(page.getByTestId('artifact-body')).toContainText('fixture-body');
  await expect(page.getByText('不属于所选方向')).toBeVisible();
  await page.getByRole('tab', { name: '事实' }).click();
  await expect(page.getByTestId('fact-list')).toContainText('入口返回 200');
  await page.getByRole('tab', { name: '发现' }).click();
  await expect(page.getByTestId('finding-row')).toContainText('confirmed');
  await expect(page.getByTestId('finding-row')).toContainText('复测 无');
  await page.getByRole('tab', { name: '目标' }).click();
  await expect(page.getByTestId('goal-row')).toContainText('open');
  await captionShot(page, 'contract-fixture-inspector');
});

test('kill changes only the selected direction', async ({ page }) => {
  await page.goto(runPath(RUN, WORK[0]));
  await expect(page.getByTestId('kill-direction')).toBeEnabled();
  const requestPromise = page.waitForRequest((request) => request.method() === 'POST' && request.url().includes('/commands'));
  await page.getByTestId('kill-direction').click();
  const request = await requestPromise;
  const body = JSON.parse(request.postData() || '{}') as { type?: string; work_item_id?: string; expected_epoch?: number; role?: string };
  expect(body.type).toBe('kill_work');
  expect(body.work_item_id).toBe(WORK[0]);
  expect(body.expected_epoch).toBe(2);
  expect(body.role).toBeUndefined();
  await expect(page.getByTestId('command-feedback')).toContainText('delivered=false');
  await expect(page.locator(`[data-work-item-id="${WORK[0]}"]`)).toContainText('killed');
  await expect(page.locator(`[data-work-item-id="${WORK[1]}"]`)).toContainText('running');
});

test('pause still allows stop, then a new epoch is separate', async ({ page }) => {
  await page.goto(runPath());
  await page.getByTestId('pause-run').click();
  await expect(page.getByTestId('run-status')).toContainText('paused');
  await expect(page.getByTestId('stop-run')).toBeEnabled();
  await expect(page.getByTestId('pause-run')).toBeDisabled();
  await page.getByTestId('stop-run').click();
  await expect(page.getByTestId('run-status')).toContainText('stopped');
  await expect(page.getByTestId('run-epoch')).toContainText('Epoch 2');
  await page.getByTestId('epoch-reason').fill('停止后继续探索');
  await page.getByTestId('start-epoch').click();
  await expect(page.getByTestId('run-epoch')).toContainText('Epoch 3');
  await expect(page.getByTestId('run-status')).toContainText('running');
});

test('temporary provider interruption explains recovery without claiming success', async ({ page }) => {
  await page.route('**/api/runs/**', async (route) => {
    const path = new URL(route.request().url()).pathname;
    if (path !== `/api/runs/${RUN}` && path !== `/api/runs/${RUN}/workbench`) {
      await route.continue();
      return;
    }
    const response = await route.fetch();
    const body = await response.json();
    if (path.endsWith('/workbench')) {
      body.run.status = 'stopped';
    } else {
      body.status = 'stopped';
      body.finish_reason = 'needs_input';
      body.final_output = '模型服务暂时不可用，已有结果已保留。\n\nEvidence inventory';
    }
    await route.fulfill({ response, json: body });
  });
  await page.goto(runPath());
  await expect(page.getByTestId('run-status')).toContainText('stopped');
  await expect(page.getByTestId('run-termination-notice')).toContainText('模型服务暂时不可用');
  await page.getByTestId('epoch-reason').fill('模型服务恢复后继续');
  await expect(page.getByTestId('start-epoch')).toBeEnabled();
});

test('chat submit shows pending, 202, source command, and 409', async ({ page }) => {
  await page.goto(runPath());
  await page.getByLabel('给 Main Agent 的消息').fill('请记录当前入口');
  await page.getByTestId('send-message').click();
  await expect(page.getByTestId('submission-status')).toContainText('正在提交');
  await expect(page.getByTestId('submission-status')).toContainText('HTTP 202');
  const action = page.getByTestId('message-row').filter({ hasText: '已接受用户回合并生成命令' });
  await expect(action).toContainText('action');
  await expect(action).toHaveAttribute('data-command-id', /[0-9a-f-]{36}/);
  await page.getByLabel('给 Main Agent 的消息').fill('另一条内容');
  await page.getByTestId('conflict-resend').click();
  await expect(page.getByTestId('submission-status')).toContainText('409');
  await captionShot(page, 'contract-fixture-chat-409');
});

test('refresh keeps the selected direction and disconnect keeps the last snapshot', async ({ page }) => {
  await page.goto(runPath(RUN, WORK[0]));
  await expect(page.getByTestId('inspector-objective')).toContainText('读取授权入口');
  await page.reload();
  await expect(page.getByTestId('inspector-objective')).toContainText('读取授权入口');
  await page.route('**/api/runs/**/workbench**', (route) => route.abort());
  await expect(page.getByTestId('stale-snapshot')).toBeVisible();
  await expect(page.getByTestId('direction-card')).toHaveCount(4);
  await expect(page.getByTestId('inspector-objective')).toContainText('读取授权入口');
});

test('legacy run stays readable and does not enable unified commands', async ({ page }) => {
  await page.goto(runPath(LEGACY));
  await expect(page.getByTestId('workbench-unavailable')).toContainText('not_implemented');
  await expect(page.getByTestId('legacy-record')).toContainText('不是 workbench.v1');
  await expect(page.getByTestId('legacy-work-item')).toContainText('历史入口检查');
  await expect(page.getByTestId('direction-card')).toHaveCount(0);
  await expect(page.getByTestId('pause-run')).toBeDisabled();
  await expect(page.getByTestId('stop-run')).toBeDisabled();
  await captionShot(page, 'contract-fixture-legacy');
});

test('terminal snapshot keeps status and epoch together', async ({ page }) => {
  await page.goto(runPath(TERMINAL));
  await expect(page.getByTestId('run-status')).toContainText('stopped');
  await expect(page.getByTestId('run-epoch')).toContainText('Epoch 2');
  await expect(page.getByTestId('run-status')).not.toContainText('running');
});

test('retest update appears after the snapshot time changes', async ({ page }) => {
  await page.goto(runPath(RUN, WORK[0]));
  await page.getByRole('tab', { name: '发现' }).click();
  await expect(page.getByTestId('finding-row')).toContainText('复测 无');
  const response = await fetch(`${FIXTURE}/__fixture/retest`, { method: 'POST' });
  expect(response.ok).toBeTruthy();
  await expect(page.getByTestId('finding-row')).toContainText('复测 fixed');
});

test('task query redirects to the canonical path and a run query does not drop the run', async ({ page }) => {
  await page.goto(`/workbench?task=${TASK}&run=${RUN}&work=${WORK[0]}`);
  await expect(page).toHaveURL(new RegExp(`/workbench/tasks/${TASK}/runs/${RUN}/work/${WORK[0]}$`));
  await page.goto(`/workbench?run=${RUN}`);
  await expect(page).toHaveURL(new RegExp(`/workbench\\?run=${RUN}$`));
  await expect(page.getByTestId('direction-card')).toHaveCount(4);
});

test('create form defaults to three workers and server model configuration', async ({ page }) => {
  await page.goto('/workbench');
  await page.getByTestId('open-create').click();
  await expect(page.getByTestId('creation-defaults')).toContainText('3 个并发 Worker');
  await expect(page.locator('#worker-count')).not.toBeVisible();
  await page.locator('#create-task-objective').fill('确认入口');
  await page.locator('#scope-origin').fill('http://127.0.0.1:29090');
  const requestPromise = page.waitForRequest((request) => request.method() === 'POST' && /\/api\/tasks\/[^/]+\/runs$/.test(new URL(request.url()).pathname));
  await page.getByTestId('create-task-submit').click();
  const request = await requestPromise;
  const body = JSON.parse(request.postData() || '{}') as { executor?: string; provider?: string; budget?: { max_parallel_workers?: number } };
  expect(body.executor).toBe('orchestrator');
  expect(body.budget).toBeUndefined();
  expect(body.provider).toBe('openai');
  await expect(page.getByTestId('worker-capacity')).toContainText('requested 3');
});

test('old run page links into the workbench', async ({ page }) => {
  await page.goto(`/tasks/${TASK}/runs/${RUN}`);
  await page.getByRole('link', { name: '在统一工作台打开' }).click();
  await expect(page).toHaveURL(new RegExp(`/workbench/tasks/${TASK}/runs/${RUN}$`));
  await expect(page.getByTestId('workbench-shell')).toBeVisible();
});

test('narrow layout switches panes without dropping the run', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto(runPath());
  await expect(page.getByTestId('pane-nav')).toBeVisible();
  await page.getByTestId('pane-inspect').click();
  await expect(page.getByTestId('inspector')).toBeVisible();
  await page.getByTestId('pane-execute').click();
  await expect(page.getByTestId('direction-card')).toHaveCount(4);
  await captionShot(page, 'contract-fixture-narrow');
});

test('switches to exploration canvas and displays graph nodes', async ({ page }) => {
  await page.goto(runPath());
  await expect(page.getByRole('button', { name: /探索链路画板/ })).toBeVisible();
  await page.getByRole('button', { name: /探索链路画板/ }).click();
  await expect(page.getByTestId('exploration-canvas')).toBeVisible();
  await captionShot(page, 'contract-fixture-canvas');
});
