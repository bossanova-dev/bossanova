export default {
  id: 'e2e-criterion-ids',
  rule: 'E2e: real IDs/layout, unique per surface.',
  command: 'node scripts/guards/feature-coverage.mjs lint --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/features/**/*.md', 'services/boss/e2e/**', 'services/web/e2e/**'],
  baseline: 'scripts/guards/e2e-criterion-ids-baseline.json',
  proof: {
    file: 'scripts/guards/feature-coverage.test.mjs',
    test: 'e2e criterion lint rejects an unknown criterion id',
  },
}
