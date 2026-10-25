export default {
  id: 'go-returned-interfaces',
  rule: 'Baseline Go interface returns.',
  command: 'node scripts/lint/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['**/*.go', 'scripts/guards/go-complexity.mjs', 'scripts/guards/budgets.json'],
  baseline: 'scripts/guards/go-returned-interfaces-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'scripts/guards/go-complexity.test.mjs',
    test: 'seeded ireturn violation fails the Go complexity guard',
  },
}
