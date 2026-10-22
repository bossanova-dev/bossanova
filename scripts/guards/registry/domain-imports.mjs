export default {
  id: 'domain-imports',
  rule: 'Domain packages import service packages only within internal/domain.',
  command: 'node scripts/guards/domain-imports.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/bossd/internal/domain/**/*.go',
    'services/bosso/internal/domain/**/*.go',
    '.golangci.yml',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/domain-imports.test.mjs',
    test: 'domain-imports real depguard flags seeded domain imports',
  },
}
