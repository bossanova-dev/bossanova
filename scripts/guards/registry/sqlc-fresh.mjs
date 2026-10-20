export default {
  id: 'sqlc-fresh',
  rule: 'Committed sqlc bindings match their queries and migrations.',
  command: 'make sqlc-diff',
  wiring: {
    make: 'sqlc-diff',
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
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/sqlc-gates.test.mjs',
    test: 'sqlc-fresh rejects a hand edit',
  },
}
