export default {
  id: 'main-gate-coverage',
  rule: "The scripts workflow runs on every release PR and nightly on main, and every other workflow's main exclusion is explicit.",
  command: 'node --test scripts/guards/check-main-gate-coverage.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.github/workflows/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-main-gate-coverage.test.mjs',
    test: 'a scripts workflow that drops the production release-PR trigger fails the gate',
  },
}
