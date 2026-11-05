export default {
  id: 'ci-request-sites',
  rule: 'Every published skill push requests CI or carries a reasoned skip.',
  command: 'node scripts/guards/check-ci-request-sites.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'lib/bossalib/contract/plugin/skillpayload/skills/**/*.md',
    'skills-toolbox/**/*.mjs',
    'skills-toolbox/**/*.sh',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-ci-request-sites.test.mjs',
    test: 'unaccounted tag-and-push reports its file and line, request after push accounts for it',
  },
}
