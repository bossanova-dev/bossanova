export default {
  id: 'criterion-proofs-kept',
  rule: 'Unproven criteria shrink.',
  command: 'node scripts/guards/feature-coverage.mjs proofs --check',
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
  baseline: 'scripts/guards/criterion-proofs-kept-baseline.json',
  proof: {
    file: 'scripts/guards/feature-coverage.test.mjs',
    test: "criterion proofs reject deleting a criterion's only proof",
  },
}
