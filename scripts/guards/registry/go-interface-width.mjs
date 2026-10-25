export default {
  id: 'go-interface-width',
  rule: 'Cap Go interfaces.',
  command: 'node scripts/lint/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['**/*.go', 'scripts/guards/go-complexity.mjs', 'scripts/guards/budgets.json'],
  baseline: null,
  proof: {
    file: 'scripts/guards/go-complexity.test.mjs',
    test: 'seeded interfacebloat violation fails the Go complexity guard',
  },
}
