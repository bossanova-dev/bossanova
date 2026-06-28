import { defineConfig, devices } from '@playwright/test';

// Proof/E2E config for the docs (Docusaurus) site. We build the static site and
// serve it with `docusaurus serve` against the pre-built `build/` dir — no SSR,
// no API. Mirrors services/marketing/playwright.config.ts. The bs-proof browser
// runner (scripts/proof-playwright-runner.mjs --surface docs) writes its
// generated spec into tests/e2e and runs it here.

const port = 3201;
const baseURL = `http://127.0.0.1:${port}`;

export default defineConfig({
  testDir: './tests/e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [['github'], ['html', { open: 'never' }]] : 'list',
  use: {
    baseURL,
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] },
    },
  ],
  webServer: {
    command: `pnpm run build && pnpm exec docusaurus serve --port ${port} --host 127.0.0.1 --no-open`,
    url: baseURL,
    reuseExistingServer: !process.env.CI,
    timeout: 180_000,
  },
});
