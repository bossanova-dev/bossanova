export default {
  id: 'skill-pin-locators',
  rule: 'Published cores resolve shared-tree toolboxes only in reasoned pre-pin blocks.',
  command: 'node scripts/guards/check-skill-pin-locators.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['lib/bossalib/contract/plugin/skillpayload/skills/**/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-skill-pin-locators.test.mjs',
    test: 'a shared-tree locator in a block after preflight fails at its file and line',
  },
}
