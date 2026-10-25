export default {
  id: 'go-cognitive-complexity',
  rule: 'Cap Go complexity.',
  command: 'node scripts/lint/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['**/*.go', 'scripts/guards/go-complexity.mjs', 'scripts/guards/budgets.json'],
  baseline: null,
  proof: {
    file: 'scripts/guards/go-complexity.test.mjs',
    test: 'seeded gocognit violation fails the Go complexity guard',
  },
}
