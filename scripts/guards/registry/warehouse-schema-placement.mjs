export default {
  id: 'warehouse-schema-placement',
  rule: 'Warehouse models land in their configured schemas without a target prefix.',
  command: 'make test-warehouse',
  wiring: { make: 'test-warehouse', ci: { workflow: 'test-warehouse.yml', job: 'warehouse' } },
  scans: [
    'services/warehouse/scripts/check_schema_placement.py',
    'services/warehouse/models/**/*.sql',
    'services/warehouse/dbt_project.yml',
    'services/warehouse/macros/generate_schema_name.sql',
  ],
  baseline: null,
  proof: {
    file: 'services/warehouse/tests/unit/test_schema_placement.py',
    test: 'test_target_prefixed_schema_fails',
  },
}
