export default {
  id: 'golangci-uncapped',
  rule: 'golangci-lint runs with no output caps or same-line de-duplication, so every violation reaches the baseline.',
  command: 'node scripts/guards/check-registry.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.golangci.yml'],
  baseline: null,
  proof: {
    file: 'scripts/guards/golangci-caps.test.mjs',
    test: 'checkGolangciCaps fails a seeded max-same-issues: 3',
  },
}
