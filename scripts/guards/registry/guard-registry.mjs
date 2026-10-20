export default {
  id: 'guard-registry',
  rule: 'Every registered guard names a proof test, scans at least one tracked file, and its CI job triggers on every file it scans.',
  command: 'node scripts/guards/check-registry.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/guards/**', '.github/workflows/test-scripts.yml'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-registry.test.mjs',
    test: 'checkRegistry fails a seeded record whose proof test is missing',
  },
}
