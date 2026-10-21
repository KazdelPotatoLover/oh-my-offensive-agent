import { defineConfig } from '@playwright/test';
import path from 'node:path';

const database = process.env.ARTEX_E2E_DATABASE_URL;
if (!database || !new URL(database).pathname.includes('browser') || new URL(database).pathname === '/agent_workbench') {
  throw new Error('Set ARTEX_E2E_DATABASE_URL to a dedicated browser acceptance database. The development database is forbidden.');
}

export default defineConfig({
  testDir: './tests/e2e',
  workers: 1,
  fullyParallel: false,
  timeout: 120000,
  expect: { timeout: 15000 },
  reporter: [['list'], ['json', { outputFile: 'test-results/artex-report.json' }]],
  use: { baseURL: 'http://127.0.0.1:23001', channel: 'chromium', headless: true, screenshot: 'only-on-failure', trace: 'retain-on-failure' },
  webServer: [
    { command: 'node tests/fixtures/artex-server.mjs', url: 'http://127.0.0.1:28282/health', reuseExistingServer: false },
    {
      command: 'go run ./cmd/server', cwd: path.resolve(__dirname, '../backend'),
      url: 'http://127.0.0.1:28181/health', reuseExistingServer: false,
      env: {
        DATABASE_URL: database, PORT: '28181', MIGRATIONS_PATH: './migrations',
        CORS_ORIGINS: 'http://127.0.0.1:23001', WORKSPACE_ROOT: '/tmp/omoa-artex-browser-workspaces',
        LLM_API_FORMAT: 'chat_completions', LLM_BASE_URL: 'http://127.0.0.1:28282/v1',
        LLM_API_KEY: 'local-browser-fixture-only', LLM_MODEL: 'local-browser-fixture',
      },
    },
    {
      command: 'npm run dev -- --hostname 127.0.0.1 --port 23001', url: 'http://127.0.0.1:23001/tasks', reuseExistingServer: false,
      env: { NEXT_DIST_DIR: '.next-e2e', INTERNAL_API_URL: 'http://127.0.0.1:28181', NEXT_PUBLIC_API_URL: 'http://127.0.0.1:28181' },
      timeout: 120000,
    },
  ],
});
