import { test, expect, Page, APIRequestContext } from '@playwright/test';

const backend = 'http://127.0.0.1:28181/api';
const fixture = 'http://127.0.0.1:28282';
const json = async (request: APIRequestContext, route: string) => {
  const response = await request.get(`${backend}${route}`);
  expect(response.ok(), `${route}: ${response.status()}`).toBeTruthy();
  return response.json();
};
const control = async (request: APIRequestContext, data: object) => {
  expect((await request.post(`${fixture}/control`, { data })).ok()).toBeTruthy();
};
const state = async (request: APIRequestContext) => (await request.get(`${fixture}/state`)).json();

async function startRun(page: Page, taskID: string) {
  await page.goto(`/tasks/${taskID}`);
  await page.getByRole('button', { name: '真实模型 API', exact: true }).click();
  const created = page.waitForResponse(r => r.url().endsWith(`/tasks/${taskID}/runs`) && r.request().method() === 'POST');
  await page.getByRole('button', { name: 'Start Run', exact: true }).click();
  const response = await created;
  expect(response.status(), await response.text()).toBe(201);
  const run = await response.json();
  await page.goto(`/tasks/${taskID}/runs/${run.id}`);
  return run.id as string;
}

test('UI: evidence, pause/steer/resume, review, independent retest, grant and same-Run epoch', async ({ page, request }, testInfo) => {
  const alerts: string[] = [];
  page.on('dialog', async dialog => {
    if (dialog.type() === 'alert') alerts.push(dialog.message());
    await dialog.accept();
  });
  await control(request, { reset: true });
  await page.goto('/tasks');
  await page.getByRole('button', { name: 'New Task', exact: true }).click();
  await page.locator('#task-name').fill('ARTEX browser acceptance');
  await page.locator('#task-objective').fill('Preserve authorized local HTTP evidence and prove the deliverable.');
  await page.getByRole('button', { name: /自定义目标种子/ }).click();
  await page.getByPlaceholder('http://192.168.77.88:7780/').fill('http://127.0.0.1:29191/');
  await page.getByText('自定义能力 profile（可选）', { exact: true }).click();
  await page.getByLabel('能力 profile 配置').fill(JSON.stringify([{ name: 'browser_evidence', version: '1', tools: [{ name: 'get_endpoint' }, { name: 'register_asset' }] }]));
  const taskResponse = page.waitForResponse(r => r.url().endsWith('/tasks') && r.request().method() === 'POST');
  await page.locator('form').getByRole('button', { name: 'Create Task', exact: true }).click();
  const createdTask = await taskResponse;
  expect(createdTask.status(), await createdTask.text()).toBe(201);
  const task = await createdTask.json();
  const runID = await startRun(page, task.id);
  await expect.poll(async () => (await state(request)).workerWaiting).toBe(true);

  await page.getByRole('button', { name: 'Pause (暂停)', exact: true }).click();
  await expect.poll(async () => (await json(request, `/runs/${runID}`)).status).toBe('paused');
  const beforePause = (await state(request)).fixtureRequests;
  await page.getByRole('button', { name: /Work Items \(工作列表\)/ }).click();
  await page.getByRole('button', { name: '人工纠偏 (Steer)', exact: true }).click();
  await page.getByPlaceholder('输入指导意见，例如：优先测试 /admin 路径下的导出接口...').fill('Keep the original objective; preserve the local marker and exact evidence source.');
  await page.getByRole('button', { name: '提交纠偏', exact: true }).click();
  await expect(page.getByRole('button', { name: '提交纠偏', exact: true })).toHaveCount(0);
  await control(request, { release: true });
  // The held provider response has returned, but paused admission must prevent its tool call.
  await expect.poll(async () => (await state(request)).workerWaiting).toBe(false);
  expect((await state(request)).fixtureRequests).toBe(beforePause);
  await page.getByRole('button', { name: 'Resume (恢复)', exact: true }).click();
  await expect.poll(async () => (await json(request, `/runs/${runID}`)).status, { timeout: 30000 }).toBe('completed');
  await expect.poll(async () => (await state(request)).sawSteer).toBe(true);
  const { findings } = await json(request, `/runs/${runID}/findings`);
  expect(findings).toHaveLength(1);
  expect(findings[0].verification_state).toBe('verified');
  expect((await json(request, `/runs/${runID}/goals`)).goals[0].state).toBe('met');
  const { assets } = await json(request, `/runs/${runID}/assets`);
  expect(assets.some((a: { kind: string }) => a.kind === 'host')).toBe(true);

  await page.getByRole('button', { name: /能力、完整产物与探索关系/ }).click();
  const resources = page.getByTestId('exploration-resources');
  await expect(resources.getByText('browser_evidence · v1')).toBeVisible();
  await resources.getByRole('button', { name: /tool-output/ }).first().click();
  await expect(resources.getByText(/已读至/)).toBeVisible();
  await resources.getByRole('button', { name: /^goal ·/ }).click();
  await expect(resources.getByText('proves', { exact: true })).toBeVisible();
  await resources.getByLabel('证明证据 ID').fill(findings[0].id);
  await resources.getByLabel('证明说明').fill('Operator reviewed the current marker response and its preserved source.');
  await resources.getByRole('button', { name: '提交证据验证目标', exact: true }).click();
  await expect(resources.getByRole('alert')).toHaveCount(0);
  await page.getByRole('button', { name: /目标与探索图 \(Goals & Graph\)/ }).click();
  await expect(page.getByText('已证明 (Proved)', { exact: true })).toBeVisible();
  await resources.getByLabel('人工线索', { exact: true }).fill('Keep the original response and review history when verifying a fix.');
  await resources.getByLabel('线索关联资产').fill('invalid-asset-uuid');
  await resources.getByRole('button', { name: '提交人工线索', exact: true }).click();
  await expect(resources.getByRole('alert')).toContainText('invalid asset_id');
  await resources.getByLabel('线索关联资产').fill(assets.find((a: { kind: string }) => a.kind === 'http_endpoint').id);
  await resources.getByRole('button', { name: '提交人工线索', exact: true }).click();
  await expect(resources.getByLabel('人工线索', { exact: true })).toHaveValue('');
  await expect(resources.getByRole('alert')).toHaveCount(0);
  await page.screenshot({ path: testInfo.outputPath('evidence-and-proof.png'), fullPage: true });

  await page.getByRole('button', { name: '进行人工审核', exact: true }).click();
  await page.getByPlaceholder('例如：Alice').fill('Browser auditor');
  await page.getByPlaceholder(/请详述依据/).fill('Confirmed the saved local fixture response and exact marker condition.');
  await page.getByRole('button', { name: '确认并更新审核结论', exact: true }).click();
  await expect.poll(async () => (await json(request, `/runs/${runID}/findings`)).findings[0].status).toBe('confirmed');
  await control(request, { patch: true });
  await page.getByRole('button', { name: '发起独立复测', exact: true }).click();
  await page.getByPlaceholder('复测备注 (例如: 修复补丁已发布至测试环境)').fill('Local fixture changed from exposed 200 marker to protected 403 marker.');
  await page.getByRole('button', { name: '确认启动复测', exact: true }).click();
  await expect.poll(async () => {
    const result = await json(request, `/runs/${runID}/findings/${findings[0].id}/retests`);
    return result.retests?.[0]?.verdict;
  }, { timeout: 30000 }).toBe('fixed');
  await expect(page.getByText('结论: fixed', { exact: true })).toBeVisible();
  expect((await json(request, `/runs/${runID}`)).status).toBe('completed');
  expect((await json(request, `/runs/${runID}/goals`)).goals[0].state).toBe('open');
  await resources.getByRole('button', { name: '刷新资料', exact: true }).click();
  await expect(page.getByText('未达成 (Open)', { exact: true })).toBeVisible();
  expect((await json(request, `/runs/${runID}/findings`)).findings[0].status).toBe('confirmed');
  const reviews = await json(request, `/runs/${runID}/findings/${findings[0].id}/reviews`);
  expect(reviews.reviews).toHaveLength(1);
  await page.screenshot({ path: testInfo.outputPath('independent-retest.png'), fullPage: true });

  // A second execution exercises an interrupted direction continuing in the same Run.
  await control(request, { reset: true });
  const resumedRun = await startRun(page, task.id);
  await expect.poll(async () => (await state(request)).workerWaiting).toBe(true);
  const before = await json(request, `/runs/${resumedRun}/work_items`);
  const originalItem = before.work_items[0];
  await page.getByRole('button', { name: 'Pause (暂停)', exact: true }).click();
  await expect.poll(async () => (await json(request, `/runs/${resumedRun}`)).status).toBe('paused');
  await page.getByRole('button', { name: 'Stop Run', exact: true }).click();
  await expect.poll(async () => (await json(request, `/runs/${resumedRun}`)).status).toBe('stopped');
  await page.getByRole('button', { name: /执行控制与代次/ }).click();
  await page.getByRole('button', { name: '追加预算授权 (Grant Budget)', exact: true }).click();
  await page.getByRole('button', { name: '提交追加授权', exact: true }).click();
  await expect.poll(async () => (await json(request, `/runs/${resumedRun}/budget_grants`)).grants.length).toBe(1);
  await control(request, { release: true });
  await page.getByRole('button', { name: '推进新执行代次 (Start New Epoch)', exact: true }).click();
  await expect.poll(async () => {
    const run = await json(request, `/runs/${resumedRun}`);
    return `${run.current_epoch}:${run.status}`;
  }, { timeout: 30000 }).toBe('2:completed');
  const after = await json(request, `/runs/${resumedRun}/work_items`);
  expect(after.work_items).toHaveLength(1);
  expect(after.work_items[0].id).toBe(originalItem.id);
  expect(after.work_items[0].current_attempt_id).not.toBe(originalItem.current_attempt_id);
  const epochs = await json(request, `/runs/${resumedRun}/epochs`);
  expect(epochs.epochs).toHaveLength(2);
  expect(epochs.epochs[0].status).toBe('stopped');
  expect(epochs.epochs[1].status).toBe('completed');
  await expect(page.getByText(/当前代次: Epoch #2/)).toBeVisible();
  await expect(page.getByRole('button', { name: 'Stop Run', exact: true })).toHaveCount(0);
  await expect(page.getByRole('row').filter({ hasText: /Epoch\s*#2/ })).toContainText('completed');
  await page.screenshot({ path: testInfo.outputPath('same-run-epoch.png'), fullPage: true });
  expect(alerts).toEqual([]);
  await testInfo.attach('acceptance-identities', { body: JSON.stringify({ taskID: task.id, runID, resumedRun }, null, 2), contentType: 'application/json' });
});
