export default {
  id: 'bosso-schema-names',
  rule: 'Migrated Postgres column names and types follow boolean, timestamp and date naming.',
  command:
    'BOSSO_REQUIRE_POSTGRES_TESTS=1 go -C services/bosso test ./internal/dbtest -run TestPostgresSchemaNaming -count=1 -v',
  wiring: {
    make: 'test-bosso-postgres',
    ci: {
      workflow: 'bazel.yml',
      job: 'go-test',
    },
  },
  scans: ['services/bosso/migrations_postgres/*.sql'],
  baseline: null,
  proof: {
    file: 'services/bosso/internal/dbtest/schema_naming_guard_test.go',
    test: 'TestPostgresSchemaNamingFailsOnSeededBareBoolean',
  },
}
