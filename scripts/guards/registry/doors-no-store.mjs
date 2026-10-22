export default {
  id: 'doors-no-store',
  rule: 'Service doors never import their store, its subpackages or adapters; legacy file exceptions only shrink.',
  command: 'node scripts/guards/doors-no-store.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    ...['server'].map((pkg) => `services/bosso/internal/${pkg}/**/*.go`),
    'services/bossd/**/*.go',
    'services/bosso/api/**/*.go',
    '.golangci.yml',
  ],
  baseline: 'scripts/guards/doors-no-store-baseline.json',
  proof: {
    file: 'scripts/guards/doors-no-store.test.mjs',
    test: 'doors-no-store real depguard flags seeded current and future doors',
  },
}
