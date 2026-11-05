import { defineConfig } from '@playwright/test';

// Smoke test for the dashboard against a running PathGauge server (by
// default the scripts/dev-stack.sh instance). See README.md.
export default defineConfig({
  testDir: 'tests',
  timeout: 300_000,
  workers: 1,
  reporter: [['list']],
  outputDir: 'test-results',
  expect: { timeout: 10_000 },
  use: {
    baseURL: process.env.PATHGAUGE_URL ?? 'https://127.0.0.1:17443',
    ignoreHTTPSErrors: true,
    viewport: { width: 1400, height: 900 },
    actionTimeout: 15_000,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
});
