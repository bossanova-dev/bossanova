export default {
  id: 'warehouse-growth-contract',
  rule: 'Warehouse growth marts preserve the required contract columns.',
  command: 'make test-warehouse',
  wiring: { make: 'test-warehouse', ci: { workflow: 'test-warehouse.yml', job: 'warehouse' } },
  scans: [
    'services/warehouse/tests/unit/test_growth_contract.py',
    'services/warehouse/models/growth/**/*.sql',
    'services/warehouse/data-tests/**/*.sql',
    'services/warehouse/dbt_project.yml',
  ],
  baseline: null,
  proof: {
    file: 'services/warehouse/tests/unit/test_growth_contract.py',
    test: 'test_missing_column_fires',
  },
}
