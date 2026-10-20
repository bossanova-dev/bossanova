export default {
  id: 'store-verbs',
  rule: 'Store methods and sqlc queries use canonical verbs without repeating their entity.',
  command: 'node scripts/guards/store-verbs.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: [
    'services/bossd/internal/db/*.go',
    'services/bossd/internal/db/queries/**',
    'services/bosso/internal/db/*.go',
    'services/bosso/internal/db/queries/**',
  ],
  baseline: 'scripts/guards/store-verbs-baseline.json',
  proof: {
    file: 'scripts/guards/store-verbs.test.mjs',
    test: 'store-verbs rejects seeded FindRepo and InsertRepo and names allowed replacements',
  },
}
