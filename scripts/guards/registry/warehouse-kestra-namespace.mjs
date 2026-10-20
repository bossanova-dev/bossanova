export default {
  id: 'warehouse-kestra-namespace',
  rule: 'Warehouse Kestra deployments stay in the bossanova namespace.',
  command: 'make test-warehouse',
  wiring: { make: 'test-warehouse', ci: { workflow: 'test-warehouse.yml', job: 'warehouse' } },
  scans: [
    'services/warehouse/scripts/kestra_deploy.py',
    'services/warehouse/kestra/warehouse-dbt-build.yml',
    'services/warehouse/tests/unit/test_kestra_flow.py',
  ],
  baseline: null,
  proof: {
    file: 'services/warehouse/tests/unit/test_kestra_deploy.py',
    test: 'test_madverts_namespace_is_refused',
  },
}
