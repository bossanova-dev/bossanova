export default {
  id: 'go-file-size',
  rule: 'Cap Go files.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*.go'],
  baseline: null,
  proof: {
    file: 'scripts/guards/file-size.test.mjs',
    test: 'go-file-size rejects a new over-cap source file',
  },
}
