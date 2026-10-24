export default {
  boundary: true,
  id: 'bossalib-consumers',
  rule: 'Each non-generated bossalib package has two consuming modules, except reasoned parity-test baselines.',
  command: 'node scripts/guards/bossalib-consumers.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'go.work',
    'lib/bossalib/**/*.go',
    'services/**/*.go',
    'plugins/**/*.go',
    'scripts/guards/bossalib-consumers-baseline.json',
  ],
  baseline: 'scripts/guards/bossalib-consumers-baseline.json',
  proof: {
    file: 'scripts/guards/bossalib-consumers.test.mjs',
    test: 'bossalib-consumers real go list rejects one consumer and accepts two',
  },
}
