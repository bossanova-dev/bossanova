export default {
  id: 'warehouse-withheld-model-build',
  rule: 'Warehouse builds refuse models that read withheld source columns.',
  command: 'make test-warehouse',
  wiring: { make: 'test-warehouse', ci: { workflow: 'test-warehouse.yml', job: 'warehouse' } },
  scans: [
    'services/warehouse/models/**/*.sql',
    'services/warehouse/macros/assert_relation_readable.sql',
    'services/warehouse/scripts/grant_warehouse_reads.py',
    'services/warehouse/dbt_project.yml',
  ],
  baseline: null,
  proof: {
    file: 'services/warehouse/tests/integration/test_dbt_enforcement.py',
    test: 'test_model_selecting_a_withheld_column_fails_dbt_build',
  },
}
