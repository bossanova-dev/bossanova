export default {
  id: 'test-command-manifest',
  rule: 'docs/testing/test-command-manifest.md matches what its generator derives from the Makefile and the module layout.',
  command: 'node scripts/check-test-command-manifest.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['docs/testing/test-command-manifest.md', 'Makefile', '**/go.mod'],
  baseline: null,
  proof: {
    file: 'scripts/check-test-command-manifest.test.mjs',
    test: 'a stale manifest fails the check',
  },
}
