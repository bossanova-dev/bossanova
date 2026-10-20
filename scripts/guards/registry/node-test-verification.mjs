export default {
  id: 'node-test-verification',
  rule: 'The `node --test` verification recipe in CLAUDE.md names the TAP tokens and exit codes node actually emits.',
  command: 'node --test scripts/check-node-test-verification.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['CLAUDE.md'],
  baseline: null,
  proof: {
    file: 'scripts/check-node-test-verification.test.mjs',
    test: 'node-test verification: a deliberate throw exits 1 and is counted as a TAP failure',
  },
}
