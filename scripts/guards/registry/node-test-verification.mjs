export default {
  id: 'node-test-verification',
  rule: 'Pinned node test runs expose failures, skips, todos and filtered tests through TAP output and exit codes.',
  command: 'node --test scripts/guards/check-node-test-verification.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/guards/check-node-test-verification.test.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-node-test-verification.test.mjs',
    test: 'node-test verification: a deliberate throw exits 1 and is counted as a TAP failure',
  },
}
