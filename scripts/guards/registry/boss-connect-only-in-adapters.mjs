export default {
  boundary: true,
  id: 'boss-connect-only-in-adapters',
  rule: 'Boss Connect imports stay in adapters; legacy file exceptions only shrink.',
  command: 'node scripts/guards/boss-connect-only-in-adapters.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/boss/**/*.go', '.golangci.yml'],
  baseline: 'scripts/guards/boss-connect-only-in-adapters-baseline.json',
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard boss-connect-only-in-adapters rejects Connect imports outside adapters',
  },
}
