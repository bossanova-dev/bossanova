export default {
  id: 'tsx-file-size',
  rule: 'Cap TSX files.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*.tsx'],
  baseline: null,
  proof: {
    file: 'scripts/guards/file-size.test.mjs',
    test: 'tsx-file-size rejects a new over-cap component',
  },
}
