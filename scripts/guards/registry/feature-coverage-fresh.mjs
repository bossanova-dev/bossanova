export default {
  id: 'feature-coverage-fresh',
  rule: 'Generate feature coverage.',
  command: 'node scripts/guards/feature-coverage.mjs page --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: [
    'docs/features/*.md',
    'services/boss/e2e/**',
    'services/web/e2e/**',
    'services/**/*_test.go',
    'lib/**/*_test.go',
    'plugins/**/*_test.go',
    'services/web/src/**/*.test.ts*',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/feature-coverage.test.mjs',
    test: 'feature coverage page rejects a hand edit',
  },
}
