export default {
  id: 'e2e-harness-imports',
  rule: 'Go tests import e2e harnesses only from e2e or the harness directories; legacy exceptions only shrink.',
  command: 'node scripts/guards/e2e-harness-imports.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/boss/**/*_test.go', 'services/bossd/**/*_test.go', '.golangci.yml'],
  baseline: 'scripts/guards/e2e-harness-imports-baseline.json',
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard e2e-harness-imports rejects a harness import outside e2e',
  },
}
