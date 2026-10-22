import { defineConfig } from '@playwright/test';

const baseURL = process.env.MA_ACCEPTANCE_FRONTEND_URL;
if (!baseURL) {
  throw new Error('Set MA_ACCEPTANCE_FRONTEND_URL to the isolated acceptance frontend. Do not point this config at the shared dev server.');
}

export default defineConfig({
  testDir: '.',
  testMatch: 'ma-acceptance.spec.ts',
  workers: 1,
  fullyParallel: false,
  timeout: 180000,
  expect: { timeout: 15000 },
  reporter: [['list'], ['json', { outputFile: 'test-results/ma7-acceptance.json' }]],
  use: {
    baseURL,
    channel: 'chromium',
    headless: true,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
});
