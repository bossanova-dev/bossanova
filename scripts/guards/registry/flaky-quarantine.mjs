export default {
  id: 'flaky-quarantine',
  rule: 'Every Bazel `flaky = True` target has an owned, expiring entry in `scripts/bazel/flaky-tests.json`, and every entry names a real quarantined test.',
  command: 'node scripts/check-flaky-quarantine.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/bazel/flaky-tests.json', 'scripts/bazel/ledger.json', '**/BUILD.bazel'],
  baseline: null,
  proof: {
    file: 'scripts/check-flaky-quarantine.test.mjs',
    test: 'flaky = True with no list entry fails, naming the BUILD line',
  },
}
