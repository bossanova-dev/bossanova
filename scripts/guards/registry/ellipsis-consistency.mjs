export default {
  id: 'ellipsis-consistency',
  rule: 'User-facing copy elides with the ellipsis character U+2026, never a run of three full stops.',
  command: 'node scripts/guards/check-ellipsis-consistency.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/boss/api/tui/**',
    'services/boss/internal/accountflow/**',
    'services/boss/internal/auth/**',
    'services/boss/cmd/**',
    'services/web/src/**',
    'scripts/guards/ellipsis-consistency-baseline.json',
  ],
  baseline: 'scripts/guards/ellipsis-consistency-baseline.json',
  proof: {
    file: 'scripts/guards/check-ellipsis-consistency.test.mjs',
    test: 'the rule fires on a rendered Go string',
  },
}
