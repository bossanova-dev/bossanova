export default {
  id: 'test-file-size',
  rule: 'Cap Go/TS tests.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*_test.go', '**/*.test.ts*', '**/*.spec.ts*'],
  baseline: null,
  proof: {
    file: 'scripts/guards/file-size.test.mjs',
    test: 'test-file-size rejects a new over-cap Go test',
  },
}
