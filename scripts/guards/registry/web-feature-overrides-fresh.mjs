export default {
  id: 'web-feature-overrides-fresh',
  rule: 'Web overrides match.',
  command: 'node scripts/guards/web-feature-boundaries.mjs overrides --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['services/web/biome.json', 'services/web/src/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/web-feature-boundaries.test.mjs',
    test: 'overrides rejects a hand edit and preserves Playwright restrictions',
  },
}
