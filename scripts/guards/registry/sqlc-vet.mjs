export default {
  id: 'sqlc-vet',
  rule: 'SQL queries prepare against migrated databases.',
  command: 'make sqlc-vet',
  wiring: {
    make: 'sqlc-vet',
    ci: {
      workflow: 'bazel.yml',
      job: 'sqlc',
    },
  },
  scans: [
    'services/bossd/sqlc.yaml',
    'services/bossd/migrations/*.sql',
    'services/bossd/internal/db/queries/**',
    'services/bossd/internal/db/sqlcgen/**',
    'services/bosso/sqlc.yaml',
    'services/bosso/migrations_postgres/*.sql',
    'services/bosso/internal/db/queries/**',
    'services/bosso/internal/db/sqlcgen/**',
    'services/bossd/sqlc.vet.yaml',
    'services/bosso/sqlc.vet.yaml',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/sqlc-gates.test.mjs',
    test: 'sqlc-vet rejects a broken SQLite query',
  },
}
