import { defineConfig } from '@playwright/test';

const fixturePort = process.env.WORKBENCH_FIXTURE_PORT || '23196';
const appPort = process.env.WORKBENCH_APP_PORT || '23186';

export default defineConfig({
  testDir: './tests/workbench',
  testMatch: '**/*.spec.ts',
  workers: 1,
  fullyParallel: false,
  timeout: 60000,
  expect: { timeout: 12000 },
  reporter: [['list']],
  outputDir: 'test-results/workbench',
  use: {
    baseURL: `http://127.0.0.1:${appPort}`,
    channel: 'chromium',
    headless: true,
    viewport: { width: 1280, height: 800 },
  },
  webServer: [
    {
      command: 'node tests/workbench/fixture-server.mjs',
      url: `http://127.0.0.1:${fixturePort}/health`,
      reuseExistingServer: false,
      env: { WORKBENCH_FIXTURE_PORT: fixturePort },
    },
    {
      command: `npm run dev -- --hostname 127.0.0.1 --port ${appPort}`,
      url: `http://127.0.0.1:${appPort}/workbench`,
      reuseExistingServer: false,
      timeout: 120000,
      env: {
        NEXT_DIST_DIR: '.next-e2e',
        INTERNAL_API_URL: `http://127.0.0.1:${fixturePort}`,
        NEXT_PUBLIC_API_URL: `http://127.0.0.1:${fixturePort}`,
      },
    },
  ],
});
