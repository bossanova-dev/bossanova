export default {
  id: 'legacy-criteria-ceiling',
  rule: 'Legacy AC ceilings shrink.',
  command: 'node scripts/guards/feature-map.mjs index --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/features/*.md'],
  baseline: 'scripts/guards/legacy-criteria-ceiling-baseline.json',
  proof: {
    file: 'scripts/guards/feature-map.test.mjs',
    test: 'legacy criteria ceiling rejects a new sequential id and accepts a ticket criteria file',
  },
}
