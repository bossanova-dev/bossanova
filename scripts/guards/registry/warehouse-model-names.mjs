export default {
  id: 'warehouse-model-names',
  rule: 'Warehouse model names follow their dbt layer conventions.',
  command: 'node scripts/guards/warehouse-model-names.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['services/warehouse/models/**/*.sql'],
  baseline: 'scripts/guards/warehouse-model-names-baseline.json',
  proof: {
    file: 'scripts/guards/warehouse-model-names.test.mjs',
    test: 'warehouse-model-names fails a seeded staging source mismatch',
  },
}
