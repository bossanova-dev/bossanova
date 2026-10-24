export default {
  id: 'web-e2e-harness-imports',
  rule: 'Web Playwright imports live only under services/web/e2e or in playwright.config.ts, with shrinking baseline exceptions.',
  command: 'node scripts/guards/check-web-e2e-harness-imports.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: { workflow: 'test-scripts.yml', job: 'scripts' },
  },
  scans: [
    'services/web/biome.json',
    'services/web/playwright.config.ts',
    'services/web/src/**/*.ts',
    'services/web/src/**/*.tsx',
    'services/web/tests/**/*.ts',
    'services/web/tests/**/*.tsx',
    'services/web/e2e/**/*.ts',
    'services/web/e2e/**/*.tsx',
  ],
  baseline: 'scripts/guards/web-e2e-harness-imports-baseline.json',
  proof: {
    file: 'scripts/guards/check-web-e2e-harness-imports.test.mjs',
    test: 'checkWebE2eHarnessImports fails a seeded Playwright import outside e2e',
  },
}
