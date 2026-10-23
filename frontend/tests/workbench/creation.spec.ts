import { expect, test } from '@playwright/test';

test('two fields create a real-model task with inherited settings and an automatic name', async ({ page }) => {
  await page.goto('/workbench');
  await page.getByTestId('open-create').click();
  await expect(page.getByTestId('creation-defaults')).toContainText('3 个并发 Worker');
  const form = page.getByTestId('create-task-form');
  await expect(form.locator('input:visible, textarea:visible')).toHaveCount(2);
  await expect(page.getByLabel('总模型次数', { exact: true })).toHaveCount(0);
  await expect(page.getByLabel('Planner 预留', { exact: true })).toHaveCount(0);
  await expect(page.getByLabel('能力 profile 配置')).not.toBeVisible();
  await page.locator('#create-task-objective').fill('检查网站安全配置');
  await page.locator('#scope-origin').fill('https://example.com/app/?view=1');
  await page.screenshot({ path: '/tmp/omoa-create-simplified-2026-10-23.png' });
  const taskRequest = page.waitForRequest(r => r.method() === 'POST' && new URL(r.url()).pathname === '/api/tasks');
  const runRequest = page.waitForRequest(r => r.method() === 'POST' && /\/api\/tasks\/[^/]+\/runs$/.test(new URL(r.url()).pathname));
  await page.getByTestId('create-task-submit').click();
  const task = (await taskRequest).postDataJSON();
  const run = (await runRequest).postDataJSON();
  expect(task.name).toBe('检查网站安全配置');
  expect(task.scope.seeds).toEqual(['https://example.com/app/?view=1']);
  expect(task.config.provider).toBe('openai');
  expect(task.config).not.toHaveProperty('budget');
  expect(task.config).not.toHaveProperty('agents');
  expect(run).not.toHaveProperty('budget');
  expect(run).not.toHaveProperty('mock_scenario');
  await expect(page.getByTestId('worker-capacity')).toContainText('requested 3');
});

test('optional overrides send only concurrency and minutes, clearing fields restores inheritance', async ({ page }) => {
  await page.goto('/workbench');
  await page.getByTestId('open-create').click();
  await page.getByTestId('creation-advanced').locator('summary').first().click();
  await page.locator('#worker-count').fill('5');
  await page.locator('#run-minutes').fill('45');
  await expect(page.getByTestId('creation-defaults')).toContainText('5 个并发 Worker · 最长 45 分钟');
  await page.locator('#worker-count').fill('');
  await expect(page.getByTestId('creation-defaults')).toContainText('3 个并发 Worker');
  await page.locator('#worker-count').fill('5');
  await page.locator('#create-task-name').fill('自定任务名');
  await page.locator('#create-task-objective').fill('检查入口');
  await page.locator('#scope-origin').fill('http://127.0.0.1:29090/');
  const request = page.waitForRequest(r => r.method() === 'POST' && /\/api\/tasks\/[^/]+\/runs$/.test(new URL(r.url()).pathname));
  await page.getByTestId('create-task-submit').click();
  expect((await request).postDataJSON().budget).toEqual({ max_parallel_workers: 5, run_timeout_seconds: 2700 });
  await expect(page.getByTestId('worker-capacity')).toContainText('requested 5');
});

test('missing server model is explained and demonstration mode is an explicit choice', async ({ page }) => {
  await page.route('**/api/runtime/defaults', route => route.fulfill({ json: { budget: { max_parallel_workers: 5, run_timeout_seconds: 3600 }, model_configured: false } }));
  await page.goto('/workbench');
  await page.getByTestId('open-create').click();
  await expect(page.getByTestId('creation-defaults')).toContainText('5 个并发 Worker · 最长 60 分钟');
  await expect(page.getByText(/服务器尚未配置模型/)).toBeVisible();
  await expect(page.getByTestId('create-task-submit')).toBeDisabled();
  await page.getByTestId('creation-advanced').locator('summary').first().click();
  await page.getByText('开发与调试', { exact: true }).click();
  await page.getByLabel('执行模式', { exact: true }).selectOption('mock');
  await expect(page.getByTestId('create-task-submit')).toBeEnabled();
  await expect(page.getByTestId('creation-defaults')).toContainText('本地演示模式');
});

test('basic creation stays short on a phone and expanded options can scroll to submit', async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 700 });
  await page.goto('/workbench');
  await page.getByRole('tab', { name: '导航', exact: true }).click();
  await page.getByTestId('open-create').click();
  await expect(page.getByTestId('create-task-submit')).toBeInViewport();
  await page.getByTestId('creation-advanced').locator('summary').first().click();
  await page.getByText('开发与调试', { exact: true }).click();
  await page.getByTestId('create-task-submit').scrollIntoViewIfNeeded();
  await expect(page.getByTestId('create-task-submit')).toBeInViewport();
  await expect.poll(() => page.getByTestId('create-task-scroll').evaluate(el => el.scrollTop)).toBeGreaterThan(0);
});
