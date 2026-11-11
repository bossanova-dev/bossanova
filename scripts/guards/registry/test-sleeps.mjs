export default {
  id: 'test-sleeps',
  rule: 'Go tests sleep or start literal-duration timers only in synctest bubbles; baselined counts only shrink.',
  command: 'node scripts/guards/check-test-sleeps.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*_test.go', 'scripts/guards/test-sleeps-baseline.json'],
  baseline: 'scripts/guards/test-sleeps-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'scripts/guards/check-test-sleeps.test.mjs',
    test: 'a seeded time.Sleep in a test file fails',
  },
}
