export default {
  id: 'web-layout',
  rule: 'Web layout ratchets.',
  command: 'node scripts/guards/web-feature-boundaries.mjs layout --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['services/web/biome.json', 'services/web/src/**'],
  baseline: 'scripts/guards/web-layout-baseline.json',
  proof: {
    file: 'scripts/guards/web-feature-boundaries.test.mjs',
    test: 'layout rejects a new legacy file and update only shrinks',
  },
}
