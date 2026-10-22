import { expect, test } from '@playwright/test';

for (const viewport of [{ width: 1280, height: 600 }, { width: 900, height: 500 }, { width: 390, height: 600 }]) {
  test(`creation form scrolls to submit at ${viewport.width}x${viewport.height}`, async ({ page }) => {
    await page.setViewportSize(viewport);
    await page.goto('/workbench');
    if (viewport.width < 1024) await page.getByRole('tab', { name: '导航', exact: true }).click();
    await page.getByTestId('open-create').click();
    const panel = page.getByTestId('create-task-scroll');
    await expect(panel).toBeVisible();
    await expect.poll(() => panel.evaluate(el => el.scrollHeight > el.clientHeight && el.clientHeight > 0)).toBeTruthy();
    await page.getByTestId('create-task-submit').scrollIntoViewIfNeeded();
    await expect(page.getByTestId('create-task-submit')).toBeInViewport();
    await expect.poll(() => panel.evaluate(el => el.scrollTop)).toBeGreaterThan(0);
  });
}
