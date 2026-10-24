export default {
  id: 'vacuous-regions',
  rule: 'No raw slice-at-indexOf region extraction in scripts; use `region()` from scripts/test-infra/gate-region-lib.mjs.',
  command: 'node scripts/guards/check-vacuous-regions.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.mjs', 'skills-toolbox/**/*.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-vacuous-regions.test.mjs',
    test: 'detects the single-line raw region slice',
  },
}
