import { expect, test } from '@playwright/test';

const TASK = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const RUN = '11111111-1111-4111-8111-111111111111';
const WORK = '22222222-2222-4222-8222-222222222221';

test('canvas keeps real lineage and pending finding, refreshes detail and preserves data on errors', async ({ page }) => {
  let phase = 0;
  await page.route(`**/api/runs/${RUN}/graph`, route => {
    if (phase === 2) return route.fulfill({ status: 500, json: { error: 'graph database unavailable' } });
    return route.fulfill({ json: {
      nodes: [
        { id: 'origin-node', kind: 'fact', state: 'origin', payload: { summary: '任务目标' } },
        { id: 'goal-a', kind: 'goal', state: 'open', payload: { text: '原始目标' } },
        { id: 'goal-b', kind: 'goal', state: 'open', payload: { text: '另一个目标' } },
        { id: 'intent-node', kind: 'intent', state: 'open', work_status: 'completed', work_item_id: WORK, payload: { summary: '真正的探索方向' } },
        { id: 'fact-node', kind: 'fact', state: 'unverified', payload: { summary: '未经验证的推断' } },
        { id: 'finding-node', kind: 'finding', state: 'unverified', finding_id: 'finding-record', payload: {}, finding: { id: 'finding-record', title: phase ? '刷新后的发现标题' : '真实待审标题', status: 'pending_review', verification_state: 'unverified', source_kind: 'speculative' } },
      ],
      edges: [
        { id: 'real-yields', src: 'intent-node', dst: 'fact-node', rel: 'yields' },
        { id: 'real-derived', src: 'fact-node', dst: 'intent-node', rel: 'derived_from' },
        { id: 'origin-goal', src: 'origin-node', dst: 'goal-a', rel: 'spawns' },
        { id: 'origin-intent', src: 'origin-node', dst: 'intent-node', rel: 'derived_from' },
      ],
    } });
  });
  await page.goto(`/workbench/tasks/${TASK}/runs/${RUN}`);
  await page.getByRole('button', { name: /探索链路画板/ }).click();
  const canvas = page.getByTestId('exploration-canvas');
  await expect(canvas.locator('.react-flow__node')).toHaveCount(6);
  await expect(canvas.locator('.react-flow__edge')).toHaveCount(4);
  await expect(canvas.locator('.react-flow__node[data-id="origin-node"]')).toContainText('任务起点');
  const fact = canvas.locator('.react-flow__node[data-id="fact-node"]');
  await expect(fact).toContainText('unverified');
  const finding = canvas.locator('.react-flow__node[data-id="finding-node"]');
  await expect(finding).toContainText('真实待审标题');
  await expect(finding).toContainText('pending_review');
  await finding.click();
  const drawer = page.getByRole('dialog', { name: '节点详情' });
  await expect(drawer).toContainText('验证状态：unverified');
  await expect(drawer).toContainText('Finding ID：finding-record');
  await expect(drawer).toContainText('暂无已记录关系');
  phase = 1;
  await expect(drawer).toContainText('刷新后的发现标题', { timeout: 12000 });
  phase = 2;
  await expect(page.getByTestId('graph-notice')).toContainText('显示上次成功读取的数据', { timeout: 12000 });
  await expect(canvas.locator('.react-flow__node')).toHaveCount(6);
  await expect(drawer).toContainText('刷新后的发现标题');
});

test('initial graph failure is visible and does not fabricate nodes from workbench directions', async ({ page }) => {
  await page.route(`**/api/runs/${RUN}/graph`, route => route.fulfill({ status: 500, json: { error: 'graph unavailable' } }));
  await page.goto(`/workbench/tasks/${TASK}/runs/${RUN}`);
  await page.getByRole('button', { name: /探索链路画板/ }).click();
  await expect(page.getByTestId('graph-notice')).toContainText('graph unavailable');
  await expect(page.getByTestId('exploration-canvas').locator('.react-flow__node')).toHaveCount(0);
});
