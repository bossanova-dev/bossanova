export default {
  id: 'timing-bounds',
  rule: 'Tests avoid literal wall-clock upper bounds and child-process deadlines below 30 seconds.',
  command: 'node scripts/guards/check-timing-bounds.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*_test.go', '**/*.test.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-timing-bounds.test.mjs',
    test: 'Go bare unit literal upper bound fires',
  },
}
