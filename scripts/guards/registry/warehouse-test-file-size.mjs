export default {
  id: 'warehouse-test-file-size',
  rule: 'Warehouse test code obeys the test file size budget.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['services/warehouse/tests/**/*.py', 'services/warehouse/data-tests/**/*.sql'],
  baseline: null,
  proof: {
    file: 'scripts/guards/file-size.test.mjs',
    test: 'file-size fails a seeded over-budget Python test file',
  },
}
