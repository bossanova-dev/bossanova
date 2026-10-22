export default {
  id: 'forbidigo-bootstrap-exec',
  rule: 'The session-bootstrap packages never call a bare `exec.Command`, which no deadline can bound.',
  command: 'node scripts/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: [
    'services/bossd/internal/adapter/git/*.go',
    'services/bossd/internal/server/*.go',
    'services/bossd/internal/session/*.go',
    'services/bossd/internal/taskorchestrator/*.go',
    '.golangci.yml',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'forbidigo rejects a bare exec.Command on the session bootstrap path',
  },
}
