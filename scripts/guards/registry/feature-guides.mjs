export default {
  id: 'feature-guides',
  rule: 'Feature guide references resolve to tracked public user guides.',
  command: 'node scripts/guards/feature-map.mjs guides --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/features/*.md', 'services/docs/docs/guides/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/feature-map.test.mjs',
    test: 'feature guides reject unresolved paths',
  },
}
