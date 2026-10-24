export default {
  id: 'go-dupl',
  rule: 'No new intra-module Go clone of 100 or more tokens lands unless `scripts/guards/go-dupl-baseline.json` records it, and the baseline matches the measured pairs exactly.',
  command: 'node scripts/lint/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['**/*.go', 'scripts/guards/go-dupl-baseline.json'],
  baseline: 'scripts/guards/go-dupl-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'scripts/guards/check-go-dupl.test.mjs',
    test: 'new pair fails with module, measured count, baseline and regeneration remedy',
  },
}
