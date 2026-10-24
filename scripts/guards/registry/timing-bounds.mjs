export default {
  id: 'timing-bounds',
  rule: 'No test asserts an upper bound on measured wall-clock time against a bare numeric literal.',
  command: 'node scripts/guards/check-timing-bounds.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*_test.go', '**/*.test.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-timing-bounds.test.mjs',
    test: 'Go bare unit literal upper bound fires',
  },
}
