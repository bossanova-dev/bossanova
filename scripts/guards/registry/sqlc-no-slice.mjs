export default {
  id: 'sqlc-no-slice',
  rule: 'Postgres queries use array parameters rather than sqlc.slice.',
  command: 'node scripts/guards/sqlc-no-slice.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['services/bosso/internal/db/queries/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/sqlc-no-slice.test.mjs',
    test: 'sqlc-no-slice rejects seeded slice and names ANY replacement',
  },
}
