export default {
  id: 'store-only-sql',
  rule: 'Only stores and test database helpers import SQL packages; legacy exceptions only shrink.',
  command: 'node scripts/guards/store-only-sql.mjs --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['services/bossd/**/*.go', 'services/bosso/**/*.go', '.golangci.yml'],
  baseline: 'scripts/guards/store-only-sql-baseline.json',
  proof: {
    file: 'scripts/guards/store-only-sql.test.mjs',
    test: 'store-only-sql real depguard flags seeded server and dbx imports',
  },
}
