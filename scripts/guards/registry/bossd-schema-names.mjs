export default {
  id: 'bossd-schema-names',
  rule: 'Migrated SQLite column names and types follow boolean, timestamp and date naming; baselined legacy violations only shrink.',
  command: 'go -C services/bossd test ./internal/db -run TestSchemaNaming -count=1 -v',
  wiring: {
    make: 'test-bossd',
    ci: {
      workflow: 'bazel.yml',
      job: 'go-test',
    },
  },
  scans: ['services/bossd/migrations/*.sql'],
  baseline: 'services/bossd/internal/db/testdata/schema_naming_baseline.json',
  proof: {
    file: 'services/bossd/internal/db/schema_naming_guard_test.go',
    test: 'TestSchemaNamingFailsOnSeededBareBoolean',
  },
}
