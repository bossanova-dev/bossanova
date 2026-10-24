export default {
  id: 'flaky-quarantine',
  rule: 'Bazel, Playwright and vitest retry markers require owned, expiring entries in `scripts/bazel/flaky-tests.json` naming real quarantined tests.',
  command: 'node scripts/guards/check-flaky-quarantine.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'scripts/bazel/flaky-tests.json',
    'scripts/bazel/ledger.json',
    '**/BUILD.bazel',
    'services/web/**/*.spec.ts',
    'services/web/**/*.test.ts',
    'services/web/**/*.test.tsx',
    'services/web/vitest.config.ts',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-flaky-quarantine.test.mjs',
    test: 'flaky = True with no list entry fails, naming the BUILD line',
  },
}
