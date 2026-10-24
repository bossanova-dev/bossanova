export default {
  id: 'ts-cognitive-complexity',
  rule: 'Biome budgets.',
  command: 'node scripts/guards/check-ts-complexity.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/web/biome.json',
    'services/marketing/biome.json',
    'lib/web-helpers/biome.json',
    'services/web/**',
    'services/marketing/**',
    'lib/web-helpers/**',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-ts-complexity.test.mjs',
    test: 'ts-complexity rejects a seeded new suppression',
  },
}
