export default {
  id: 'warehouse-mart-tests',
  rule: 'Every warehouse mart and growth model has a dbt test.',
  command: 'node scripts/guards/warehouse-mart-tests.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: [
    'services/warehouse/models/**/*.sql',
    'services/warehouse/models/**/*.yml',
    'services/warehouse/data-tests/**/*.sql',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/warehouse-mart-tests.test.mjs',
    test: 'warehouse-mart-tests fails a seeded mart without a dbt test',
  },
}
