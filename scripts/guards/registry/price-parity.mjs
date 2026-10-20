export default {
  id: 'price-parity',
  rule: 'Every displayed Bossanova Cloud price derives from the one lib/web-helpers declaration and agrees across the web app, the marketing site and PRODUCT.md.',
  command: 'node scripts/check-price-parity.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'lib/web-helpers/src/**',
    'services/web/src/**',
    'services/marketing/src/**',
    'PRODUCT.md',
    'scripts/price-parity-baseline.json',
  ],
  baseline: 'scripts/price-parity-baseline.json',
  proof: {
    file: 'scripts/check-price-parity.test.mjs',
    test: 'rejects a render surface that writes the price as a literal',
  },
}
