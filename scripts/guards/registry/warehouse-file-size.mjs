export default {
  id: 'warehouse-file-size',
  rule: 'Production warehouse SQL and Python obey file size budgets; pinned budgets are the shrinking size baseline.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: [
    'services/warehouse/models/**/*.sql',
    'services/warehouse/macros/**/*.sql',
    'services/warehouse/sql/**/*.sql',
    'services/warehouse/scripts/**/*.py',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/file-size.test.mjs',
    test: 'file-size fails a seeded over-budget SQL file',
  },
}
