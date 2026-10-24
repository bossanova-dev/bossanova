export default {
  id: 'web-feature-boundaries',
  rule: 'Web boundaries hold.',
  command: 'node scripts/guards/web-feature-boundaries.mjs boundaries --check',
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
    test: 'boundaries rejects a seeded deep feature import',
  },
}
