import { expect, test, type Page } from '@playwright/test';

const backend = process.env.MA_ACCEPTANCE_BACKEND_URL;
const database = process.env.MA_ACCEPTANCE_DATABASE_URL ?? '';

test.beforeAll(() => {
  if (!backend) {
    throw new Error('Set MA_ACCEPTANCE_BACKEND_URL. This spec does not use the shared dev backend.');
  }
  if (!database.includes('agent_workbench_ma_integration') || database.endsWith('/agent_workbench') || database.includes('agent_workbench_ma7')) {
    throw new Error('MA_ACCEPTANCE_DATABASE_URL must be the integration database.');
  }
});

test('workbench: N=4, live concurrency, steer, kill, pause, grant, evidence, refresh', async ({ page, request }, testInfo) => {
  const alerts: string[] = [];
  page.on('dialog', async (dialog) => {
    alerts.push(dialog.message());
    await dialog.accept();
  });

  await page.goto('/tasks');
  await page.getByRole('button', { name: 'New Task', exact: true }).click();
  await page.locator('#task-name').fill(`ma7 acceptance ${Date.now()}`);
  await page.locator('#task-objective').fill('Inspect the authorized local lab and keep each direction separate.');
  await page.getByRole('button', { name: /自定义目标种子/ }).click();
  await page.getByPlaceholder('http://192.168.77.88:7780/').fill(process.env.MA_ACCEPTANCE_LAB_URL ?? 'http://127.0.0.1:1/');
  const created = page.waitForResponse((response) => response.url().endsWith('/api/tasks') && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Create Task', exact: true }).click();
  const taskResponse = await created;
  expect(taskResponse.status(), await taskResponse.text()).toBe(201);
  const task = await taskResponse.json();

  await page.goto(`/tasks/${task.id}`);
  await page.getByRole('button', { name: '真实模型 API', exact: true }).click();
  const workers = page.getByLabel('Worker 并发数');
  await workers.fill('4');
  const started = page.waitForResponse((response) => response.url().includes(`/tasks/${task.id}/runs`) && response.request().method() === 'POST');
  await page.getByRole('button', { name: 'Start Run', exact: true }).click();
  const runResponse = await started;
  expect(runResponse.status(), await runResponse.text()).toBe(201);
  const body = runResponse.request().postDataJSON() as { budget?: { max_parallel_workers?: number } };
  expect(body.budget?.max_parallel_workers).toBe(4);
  const run = await runResponse.json();

  await page.goto(`/tasks/${task.id}/runs/${run.id}`);
  const capacity = page.getByTestId('worker-capacity');
  await expect(capacity).toContainText(/requested\s*4/);
  await expect(capacity).toContainText(/effective\s*4/);
  await expect(capacity.getByTestId('active-workers')).toHaveText(/[2-4]/, { timeout: 20000 });

  const chat = page.getByLabel('Main Agent');
  await chat.fill('查询当前方向，不要扩大范围，也不要增加预算。');
  const posted = page.waitForResponse((response) => response.url().includes('/main/messages') && response.request().method() === 'POST');
  await page.getByRole('button', { name: '发送', exact: true }).click();
  expect((await posted).status()).toBe(202);
  await page.getByRole('button', { name: '发送', exact: true }).click();
  const replay = await request.post(`${backend}/api/runs/${run.id}/main/messages`, {
    headers: { 'Idempotency-Key': 'ma7-browser-replay' },
    data: { message: '查询当前方向，不要扩大范围，也不要增加预算。', expected_epoch: 1 },
  });
  expect(replay.status()).not.toBe(500);

  await page.getByRole('link', { name: /dir-/ }).first().click();
  await expect(page.getByTestId('direction-trace')).toBeVisible();
  await page.getByRole('button', { name: '纠偏此方向', exact: true }).click();
  await page.getByRole('button', { name: '终止此方向', exact: true }).click();
  await expect(page.getByTestId('peer-direction-status')).not.toContainText('cancelled');
  await page.getByRole('button', { name: '暂停', exact: true }).click();
  await page.getByRole('button', { name: '停止', exact: true }).click();
  await page.getByLabel('追加模型调用').fill('5');
  await page.getByRole('button', { name: '授权追加', exact: true }).click();
  await page.getByRole('button', { name: '同 Run 续跑', exact: true }).click();
  await expect(page.getByTestId('run-epoch')).toContainText('1');
  await page.getByTestId('target-evidence').click();
  await expect(page.getByTestId('final-delivery')).toBeVisible();
  await page.reload();
  await expect(page.getByTestId('worker-capacity')).toBeVisible();

  const missing = await request.get(`${backend}/api/runs/not-a-uuid/workbench`);
  expect(missing.status()).toBeGreaterThanOrEqual(400);
  const conflict = await request.post(`${backend}/api/runs/${run.id}/commands`, {
    headers: { 'Idempotency-Key': 'ma7-browser-conflict' },
    data: { type: 'hint', expected_epoch: 1, payload: { text: 'one' } },
  });
  const conflict2 = await request.post(`${backend}/api/runs/${run.id}/commands`, {
    headers: { 'Idempotency-Key': 'ma7-browser-conflict' },
    data: { type: 'stop_run', expected_epoch: 1 },
  });
  expect(conflict2.status()).toBe(409);
  expect(conflict.ok() || conflict.status() === 202).toBeTruthy();
  expect(alerts.join('\n')).not.toContain('model prose');
  await testInfo.attach('alerts', { body: alerts.join('\n'), contentType: 'text/plain' });
});

async function visibleText(page: Page, testId: string) {
  return page.getByTestId(testId).innerText();
}

void visibleText;
