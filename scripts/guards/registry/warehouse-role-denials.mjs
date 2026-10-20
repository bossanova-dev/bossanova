export default {
  id: 'warehouse-role-denials',
  rule: 'The warehouse role cannot read withheld columns.',
  command: 'make test-warehouse',
  wiring: { make: 'test-warehouse', ci: { workflow: 'test-warehouse.yml', job: 'warehouse' } },
  scans: [
    'services/warehouse/scripts/grant_warehouse_reads.py',
    'services/warehouse/tests/integration/test_warehouse_role.py',
  ],
  baseline: null,
  proof: {
    file: 'services/warehouse/tests/integration/test_warehouse_role.py',
    test: 'test_withheld_column_is_denied',
  },
}
