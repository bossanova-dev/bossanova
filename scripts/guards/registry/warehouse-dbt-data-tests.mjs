export default {
  id: 'warehouse-dbt-data-tests',
  rule: 'A failing warehouse dbt data test fails the build.',
  command: 'make test-warehouse',
  wiring: { make: 'test-warehouse', ci: { workflow: 'test-warehouse.yml', job: 'warehouse' } },
  scans: [
    'services/warehouse/models/**/*.sql',
    'services/warehouse/models/**/*.yml',
    'services/warehouse/data-tests/**/*.sql',
    'services/warehouse/dbt_project.yml',
  ],
  baseline: null,
  proof: {
    file: 'services/warehouse/tests/integration/test_dbt_enforcement.py',
    test: 'test_a_failing_data_test_fails_dbt_build',
  },
}
