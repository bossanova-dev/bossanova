export default {
  boundary: true,
  id: 'store-imports',
  rule: 'Stores import only domain types and their own subpackages; legacy upward imports only shrink.',
  command: 'node scripts/guards/store-imports.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: { workflow: 'test-scripts.yml', job: 'scripts' },
  },
  scans: ['services/bossd/internal/db/**/*.go', 'services/bosso/internal/db/**/*.go'],
  baseline: 'scripts/guards/store-imports-baseline.json',
  proof: {
    file: 'scripts/guards/store-imports.test.mjs',
    test: 'store-imports rejects seeded upward imports in either store',
  },
}
