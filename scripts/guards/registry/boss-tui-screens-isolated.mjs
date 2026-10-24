export default {
  boundary: true,
  id: 'boss-tui-screens-isolated',
  rule: 'Boss TUI screens communicate through ui messages and never import the router or another screen.',
  command: 'node scripts/guards/boss-tui-screens-isolated.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/boss/api/tui/**/*.go', '.golangci.yml'],
  baseline: 'scripts/guards/boss-tui-screens-isolated-baseline.json',
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard boss-tui-screens-isolated rejects router and screen imports while allowing ui',
  },
}
