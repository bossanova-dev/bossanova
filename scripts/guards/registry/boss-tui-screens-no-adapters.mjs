export default {
  boundary: true,
  id: 'boss-tui-screens-no-adapters',
  rule: 'Boss TUI screens declare consumer interfaces and never import process, daemon or upgrade adapters.',
  command: 'node scripts/guards/boss-tui-screens-no-adapters.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/boss/api/tui/**/*.go', '.golangci.yml'],
  baseline: 'scripts/guards/boss-tui-screens-no-adapters-baseline.json',
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard boss-tui-screens-no-adapters rejects process and adapter imports',
  },
}
