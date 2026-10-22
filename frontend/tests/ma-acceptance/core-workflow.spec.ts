import { expect, test } from '@playwright/test';
const backend = process.env.MA_ACCEPTANCE_BACKEND_URL!;
const fixture = process.env.MA_ACCEPTANCE_LAB_URL!;

test('create through workbench with server defaults, observe a real worker, and stop', async ({ page, request }) => {
  if (!fixture) throw new Error('Set MA_ACCEPTANCE_LAB_URL to a local fixture.');
  // Compile the dynamic Next route before starting a short-lived task.
  await page.goto('/workbench/tasks/00000000-0000-4000-8000-000000000001/runs/00000000-0000-4000-8000-000000000002');
  await page.goto('/workbench');
  await page.getByTestId('open-create').click();
  await page.locator('#create-task-name').fill(`Core browser ${Date.now()}`);
  await page.locator('#create-task-objective').fill('Read the authorized slow endpoint and report its status.');
  await page.locator('#scope-origin').fill(fixture);
  await page.locator('#seed-path').fill('/slow');
  await page.locator('#worker-count').fill('2');
  const started = page.waitForResponse(r => /\/api\/tasks\/[^/]+\/runs$/.test(new URL(r.url()).pathname) && r.request().method() === 'POST');
  await page.getByTestId('create-task-submit').click();
  const response = await started;
  expect(response.status(), await response.text()).toBe(201);
  const run = await response.json();
  await expect(page).toHaveURL(new RegExp(`/runs/${run.id}$`));
  await expect(page.getByTestId('worker-capacity')).toContainText('requested 2');
  await expect(page.getByTestId('direction-card').first()).toBeVisible();
  const stop = page.getByTestId('stop-run');
  await expect(stop).toBeEnabled();
  const stopped = page.waitForResponse(r => r.url().endsWith(`/runs/${run.id}/commands`) && r.request().method() === 'POST');
  await stop.click();
  expect((await stopped).ok()).toBeTruthy();
  await expect.poll(async () => (await (await request.get(`${backend}/api/runs/${run.id}`)).json()).status).toBe('stopped');
  await page.reload();
  await expect(page.getByTestId('worker-capacity')).toContainText('active 0');
});

test('real run directions and results survive refresh', async ({ page }) => {
  const task = process.env.MA_CORE_TASK_ID;
  const run = process.env.MA_CORE_RUN_ID;
  test.skip(!task || !run, 'Provide the real model smoke task/run IDs to inspect persisted results.');
  await page.goto(`/workbench/tasks/${task}/runs/${run}`);
  await expect(page.getByTestId('worker-capacity')).toContainText('requested 2');
  await expect.poll(() => page.getByTestId('direction-card').count()).toBeGreaterThanOrEqual(2);
  await page.getByTestId('direction-card').first().getByRole('button').first().click();
  await page.reload();
  await expect.poll(() => page.getByTestId('direction-card').count()).toBeGreaterThanOrEqual(2);
  await expect(page.getByTestId('main-conversation')).toBeVisible();
});
