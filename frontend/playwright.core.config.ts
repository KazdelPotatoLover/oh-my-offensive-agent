import { defineConfig } from '@playwright/test';
if (!process.env.MA_ACCEPTANCE_FRONTEND_URL || !process.env.MA_ACCEPTANCE_BACKEND_URL) {
  throw new Error('Set isolated MA_ACCEPTANCE_FRONTEND_URL and MA_ACCEPTANCE_BACKEND_URL.');
}
export default defineConfig({
  testDir: './tests/ma-acceptance', testMatch: ['core-workflow.spec.ts', 'workbench-scroll.spec.ts'], workers: 1,
  timeout: 60000, expect: { timeout: 15000 }, reporter: [['list']],
  outputDir: 'test-results/core',
  use: { baseURL: process.env.MA_ACCEPTANCE_FRONTEND_URL, channel: 'chromium', headless: true, screenshot: 'only-on-failure', trace: 'retain-on-failure' },
});
