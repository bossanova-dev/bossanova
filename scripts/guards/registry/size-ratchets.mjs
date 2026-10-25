export default {
  id: 'size-ratchets',
  rule: 'An exact-size pin on a committed artifact fails in both directions: over the pin is a regression, under it is an unbanked saving.',
  command: 'node --test scripts/test-infra/size-ratchet-lib.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/test-infra/size-ratchet-lib.mjs', 'lib/bossalib/kit/config/config.go'],
  baseline: null,
  proof: {
    file: 'scripts/test-infra/size-ratchet-lib.test.mjs',
    test: 'assertExactSize over the pin names measurement, pin, constant and file',
  },
}
