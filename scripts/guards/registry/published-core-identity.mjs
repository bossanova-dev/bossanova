export default {
  id: 'published-core-identity',
  rule: 'The toolbox sources vendored into published `boss-*` cores, and the cores themselves, carry no Bossanova identity (`BOS-123`, `bossanova-*`, …).',
  command: 'node scripts/guards/check-published-core-identity.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'lib/bossalib/contract/plugin/skillpayload/skills_manifest_test.go',
    'lib/bossalib/contract/plugin/skillpayload/skills/**',
    'skills-toolbox/**/*.mjs',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-published-core-identity.test.mjs',
    test: 'a ticket id in a vendored toolbox source fails at its file and line',
  },
}
